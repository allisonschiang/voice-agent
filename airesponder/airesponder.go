// Package airesponder implements the third stage of the voice pipeline:
// it receives a text input, optionally augments with context fetched from a
// configured service, calls Anthropic Claude, and optionally speaks the
// result via a TTS service + audio output.
//
// Extracted from the original audio-to-model/recorder module — this is the
// Claude + TTS half, with the audio capture and STT stripped out.
package airesponder

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	anthropicopt "github.com/anthropics/anthropic-sdk-go/option"

	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/resource"
	generic "go.viam.com/rdk/services/generic"
)

var Model = resource.NewModel("allisonorg", "voice-agent", "ai-responder")

const defaultPrompt = "Analyze the following transcript:"

// Config describes the LLM + TTS inputs.
type Config struct {
	AnthropicAPIKey string `json:"anthropic_api_key,omitempty"`
	Prompt          string `json:"prompt,omitempty"`
	Model           string `json:"model,omitempty"`
	MaxTokens       int    `json:"max_tokens,omitempty"`

	// Optional: service that supplies dynamic context inserted into the prompt.
	ContextService string                 `json:"context_service,omitempty"`
	ContextCommand map[string]interface{} `json:"context_command,omitempty"`
	ContextField   string                 `json:"context_field,omitempty"`

	// Optional: spoken response. TTS module owns its own API key and speaker
	// (audio_output) — ai-responder just hands it text via {"say": ...}.
	TTS        string `json:"tts,omitempty"`         // generic service, receives {"say": "<text>"}
	AudioInput string `json:"audio_input,omitempty"` // if set, pause/resume wake-word detection around TTS playback
}

// Validate returns dependencies.
func (c *Config) Validate(path string) ([]string, []string, error) {
	var deps []string
	if c.ContextService != "" {
		deps = append(deps, "rdk:service:generic/"+c.ContextService)
	}
	if c.TTS != "" {
		deps = append(deps, "rdk:service:generic/"+c.TTS)
	}
	if c.AudioInput != "" {
		deps = append(deps, c.AudioInput)
	}
	return deps, nil, nil
}

func init() {
	resource.RegisterService(generic.API, Model, resource.Registration[resource.Resource, *Config]{
		Constructor: newResponder,
	})
}

type responder struct {
	resource.Named
	logger logging.Logger

	mu sync.Mutex

	anthropicClient *anthropic.Client
	prompt          string
	model           string
	maxTokens       int

	contextService resource.Resource
	contextCommand map[string]interface{}
	contextField   string

	tts     resource.Resource
	audioIn resource.Resource

	lastInput  string
	lastResult string
}

func newResponder(
	ctx context.Context,
	deps resource.Dependencies,
	conf resource.Config,
	logger logging.Logger,
) (resource.Resource, error) {
	r := &responder{
		Named:  conf.ResourceName().AsNamed(),
		logger: logger,
	}
	if err := r.Reconfigure(ctx, deps, conf); err != nil {
		return nil, err
	}
	return r, nil
}

func (r *responder) Reconfigure(ctx context.Context, deps resource.Dependencies, conf resource.Config) error {
	cfg, err := resource.NativeConfig[*Config](conf)
	if err != nil {
		return err
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	r.prompt = cfg.Prompt
	if r.prompt == "" {
		r.prompt = defaultPrompt
	}
	r.model = cfg.Model
	if r.model == "" {
		r.model = string(anthropic.ModelClaudeHaiku4_5)
	}
	r.maxTokens = cfg.MaxTokens
	if r.maxTokens <= 0 {
		r.maxTokens = 256
	}

	var anthropicOpts []anthropicopt.RequestOption
	if cfg.AnthropicAPIKey != "" {
		anthropicOpts = append(anthropicOpts, anthropicopt.WithAPIKey(cfg.AnthropicAPIKey))
	}
	client := anthropic.NewClient(anthropicOpts...)
	r.anthropicClient = &client

	if cfg.ContextService != "" {
		r.contextService, err = generic.FromProvider(deps, cfg.ContextService)
		if err != nil {
			return fmt.Errorf("could not get context service %q: %w", cfg.ContextService, err)
		}
		r.contextCommand = cfg.ContextCommand
		r.contextField = cfg.ContextField
	} else {
		r.contextService = nil
		r.contextCommand = nil
		r.contextField = ""
	}

	if cfg.TTS != "" {
		r.tts, err = generic.FromProvider(deps, cfg.TTS)
		if err != nil {
			return fmt.Errorf("could not get TTS service %q: %w", cfg.TTS, err)
		}
	} else {
		r.tts = nil
	}

	if cfg.AudioInput != "" {
		r.audioIn, err = generic.FromProvider(deps, cfg.AudioInput)
		if err != nil {
			// Not fatal — we only use it to pause/resume wake detection.
			r.logger.Warnf("ai-responder: cannot resolve audio_input %q: %v (TTS will not pause detection)", cfg.AudioInput, err)
			r.audioIn = nil
		}
	} else {
		r.audioIn = nil
	}

	return nil
}

// DoCommand surface:
//
//	{"process": "<text>"}  → run the full pipeline (Claude → optional TTS) and return {result, spoken}
//	{"ask": "<text>"}      → alias of "process"
//	{"result": true}       → return the last result string
//	{"speak": "<text>"}    → speak text directly via TTS (bypass Claude)
func (r *responder) DoCommand(ctx context.Context, cmd map[string]interface{}) (map[string]interface{}, error) {
	if text, ok := cmd["process"].(string); ok && text != "" {
		return r.doProcess(ctx, text)
	}
	if text, ok := cmd["ask"].(string); ok && text != "" {
		return r.doProcess(ctx, text)
	}
	if text, ok := cmd["speak"].(string); ok && text != "" {
		if err := r.speak(ctx, text); err != nil {
			return nil, err
		}
		return map[string]interface{}{"status": "ok", "spoken": text}, nil
	}
	if v, ok := cmd["result"].(bool); ok && v {
		r.mu.Lock()
		result := r.lastResult
		r.mu.Unlock()
		if result == "" {
			return nil, fmt.Errorf("no result available")
		}
		return map[string]interface{}{"status": "ok", "result": result}, nil
	}
	return nil, fmt.Errorf("ai-responder: expected one of {process, ask, speak, result} in command")
}

func (r *responder) doProcess(ctx context.Context, text string) (map[string]interface{}, error) {
	r.mu.Lock()
	r.lastInput = text
	r.mu.Unlock()

	result, err := r.callClaude(ctx, text)
	if err != nil {
		return nil, err
	}

	r.mu.Lock()
	r.lastResult = result
	tts := r.tts
	r.mu.Unlock()

	spoken := false
	if tts != nil && result != "" {
		if err := r.speak(ctx, result); err != nil {
			r.logger.Warnf("ai-responder: TTS failed: %v", err)
		} else {
			spoken = true
		}
	}

	return map[string]interface{}{
		"status": "ok",
		"result": result,
		"spoken": spoken,
	}, nil
}

func (r *responder) callClaude(pipelineCtx context.Context, transcript string) (string, error) {
	r.mu.Lock()
	prompt := r.prompt
	client := r.anthropicClient
	model := r.model
	maxTokens := r.maxTokens
	ctxSvc := r.contextService
	ctxCmd := r.contextCommand
	ctxField := r.contextField
	r.mu.Unlock()

	if client == nil {
		return "", fmt.Errorf("no Anthropic client configured")
	}

	ctx, cancel := context.WithTimeout(pipelineCtx, 60*time.Second)
	defer cancel()

	contextStr := ""
	if ctxSvc != nil && ctxCmd != nil {
		result, err := ctxSvc.DoCommand(ctx, ctxCmd)
		if err != nil {
			r.logger.Warnf("ai-responder: context fetch failed: %v", err)
		} else {
			var toMarshal interface{} = result
			if ctxField != "" {
				if val, ok := result[ctxField]; ok {
					toMarshal = val
				} else {
					r.logger.Warnf("ai-responder: context_field %q not in response", ctxField)
				}
			}
			if b, err := json.Marshal(toMarshal); err == nil {
				contextStr = string(b)
				r.logger.Infof("ai-responder: context: %s", contextStr)
			}
		}
	}

	finalPrompt := prompt
	if contextStr != "" {
		if strings.Contains(finalPrompt, "{context}") {
			finalPrompt = strings.Replace(finalPrompt, "{context}", contextStr, 1)
		} else {
			finalPrompt = finalPrompt + "\n\nCurrent context:\n" + contextStr
		}
	}

	userContent := finalPrompt + "\n\n" + transcript

	msg, err := client.Messages.New(ctx, anthropic.MessageNewParams{
		Model:     anthropic.Model(model),
		MaxTokens: int64(maxTokens),
		System: []anthropic.TextBlockParam{
			{Text: "You are a concise voice assistant. Respond in 1-2 short sentences. Never use lists, markdown, or formatting."},
		},
		Messages: []anthropic.MessageParam{
			anthropic.NewUserMessage(anthropic.NewTextBlock(userContent)),
		},
	})
	if err != nil {
		return "", fmt.Errorf("Claude API call failed: %w", err)
	}

	var parts []string
	for _, block := range msg.Content {
		if block.Type == "text" {
			parts = append(parts, block.Text)
		}
	}
	result := strings.Join(parts, "\n")
	r.logger.Infof("ai-responder: Claude: %s", result)
	return result, nil
}

func (r *responder) speak(ctx context.Context, text string) error {
	r.mu.Lock()
	tts := r.tts
	audioIn := r.audioIn
	r.mu.Unlock()

	if tts == nil {
		return fmt.Errorf("no TTS configured")
	}

	// Pause wake detection so TTS output doesn't retrigger the mic.
	if audioIn != nil {
		if _, err := audioIn.DoCommand(ctx, map[string]interface{}{"pause_detection": nil}); err != nil {
			r.logger.Warnf("ai-responder: pause_detection failed: %v", err)
		}
	}

	if _, err := tts.DoCommand(ctx, map[string]interface{}{"say": text}); err != nil {
		if audioIn != nil {
			_, _ = audioIn.DoCommand(ctx, map[string]interface{}{"resume_detection": nil})
		}
		return fmt.Errorf("TTS failed: %w", err)
	}

	if audioIn != nil {
		if _, err := audioIn.DoCommand(ctx, map[string]interface{}{"resume_detection": nil}); err != nil {
			r.logger.Warnf("ai-responder: resume_detection failed: %v", err)
		}
	}

	return nil
}

func (r *responder) Close(ctx context.Context) error {
	return nil
}
