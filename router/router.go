// Package router implements a config-driven string-to-DoCommand router.
//
// Given an input string (a voice transcript, a chat message, a CLI
// argument, an MQTT payload — anything), the router looks it up in a
// config-driven phrase table and dispatches the matching DoCommand to a
// configured target resource. It is intentionally domain-agnostic: nothing
// about it requires audio. It was built for a voice pipeline but works
// equally well for any free-form string → action mapping.
//
// Config surface:
//
//   routes       []{say, target, do, ack?, ack_failure?}
//   fallback     optional resource that receives unmatched inputs as
//                {"process": "<text>"} — typically an LLM for graceful
//                "I didn't understand" handling.
//   ack_target   optional resource that receives {"speak": "<ack>"}
//                after a successful (or failed) dispatch — typically a
//                TTS-capable service. Skip if you don't want spoken
//                acknowledgments.
//
// DoCommand surface:
//
//   {"transcript": "wipe"}  or  {"input": "wipe"}
//       Match and dispatch. Returns {handled, matched, target, action,
//       response, spoken}.
//
//   {"test": "wipe"}
//       Dry run — report which route WOULD match without dispatching.
//       Handy for debugging route tables before any real traffic.
//
//   {"list-phrases": true}
//       Return the configured phrase table.
//
// Matching is case-insensitive exact match after whitespace trim. No
// substring, no fuzzy. This is deliberate: easy to reason about, no
// surprise triggers. Upgrade to fuzzy matching or an LLM intent
// classifier by swapping this module, not by reconfiguring it.
package router

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/resource"
	generic "go.viam.com/rdk/services/generic"
)

var Model = resource.NewModel("allisonorg", "voice-agent", "text-router")

// Route maps an input phrase to a DoCommand payload on a target resource.
type Route struct {
	Say        string                 `json:"say"`                   // input phrase (case-insensitive, exact match after trim)
	Target     string                 `json:"target"`                // name of the target generic service (the one that receives Do)
	Do         map[string]interface{} `json:"do"`                    // DoCommand payload sent to Target on match
	Ack        string                 `json:"ack,omitempty"`         // optional spoken confirmation after successful dispatch
	AckFailure string                 `json:"ack_failure,omitempty"` // optional spoken message if target.DoCommand returned an error
}

// Config declares the phrase table, optional LLM fallback, and optional
// acknowledgment speaker.
type Config struct {
	Routes []Route `json:"routes"`

	// Fallback: generic service that receives unmatched transcripts as
	// {"process": "<text>"}. Typically an ai-responder. Optional.
	Fallback string `json:"fallback,omitempty"`

	// AckTarget: generic service that receives {"speak": "<ack>"} after a
	// successful route dispatch. Typically the same ai-responder (its
	// "speak" DoCommand handles TTS directly). If unset, no ack is spoken.
	AckTarget string `json:"ack_target,omitempty"`
}

// Validate enforces route shape and returns the dependencies (Target + Fallback resources).
func (c *Config) Validate(path string) ([]string, []string, error) {
	seenDeps := map[string]struct{}{}
	var deps []string
	for i, r := range c.Routes {
		if strings.TrimSpace(r.Say) == "" {
			return nil, nil, fmt.Errorf("%s: route[%d].say is empty", path, i)
		}
		if strings.TrimSpace(r.Target) == "" {
			return nil, nil, fmt.Errorf("%s: route[%d].target is empty", path, i)
		}
		if r.Do == nil {
			return nil, nil, fmt.Errorf("%s: route[%d].do is missing", path, i)
		}
		dep := "rdk:service:generic/" + r.Target
		if _, ok := seenDeps[dep]; !ok {
			seenDeps[dep] = struct{}{}
			deps = append(deps, dep)
		}
	}
	if c.Fallback != "" {
		dep := "rdk:service:generic/" + c.Fallback
		if _, ok := seenDeps[dep]; !ok {
			seenDeps[dep] = struct{}{}
			deps = append(deps, dep)
		}
	}
	if c.AckTarget != "" {
		dep := "rdk:service:generic/" + c.AckTarget
		if _, ok := seenDeps[dep]; !ok {
			deps = append(deps, dep)
		}
	}
	return deps, nil, nil
}

func init() {
	resource.RegisterService(generic.API, Model, resource.Registration[resource.Resource, *Config]{
		Constructor: newRouter,
	})
}

type router struct {
	resource.Named
	resource.AlwaysRebuild
	logger logging.Logger

	mu        sync.RWMutex
	routes    []compiledRoute
	targets   map[string]resource.Resource
	fallback  resource.Resource
	ackTarget resource.Resource
}

type compiledRoute struct {
	say        string // lowercased, trimmed
	target     string
	do         map[string]interface{}
	ack        string
	ackFailure string
}

func newRouter(ctx context.Context, deps resource.Dependencies, conf resource.Config, logger logging.Logger) (resource.Resource, error) {
	cfg, err := resource.NativeConfig[*Config](conf)
	if err != nil {
		return nil, err
	}

	r := &router{
		Named:   conf.ResourceName().AsNamed(),
		logger:  logger,
		targets: map[string]resource.Resource{},
	}

	for _, route := range cfg.Routes {
		r.routes = append(r.routes, compiledRoute{
			say:        strings.ToLower(strings.TrimSpace(route.Say)),
			target:     route.Target,
			do:         route.Do,
			ack:        route.Ack,
			ackFailure: route.AckFailure,
		})
		if _, loaded := r.targets[route.Target]; loaded {
			continue
		}
		res, err := generic.FromProvider(deps, route.Target)
		if err != nil {
			return nil, fmt.Errorf("cannot resolve target %q: %w", route.Target, err)
		}
		r.targets[route.Target] = res
	}

	if cfg.Fallback != "" {
		res, err := generic.FromProvider(deps, cfg.Fallback)
		if err != nil {
			return nil, fmt.Errorf("cannot resolve fallback %q: %w", cfg.Fallback, err)
		}
		r.fallback = res
	}

	if cfg.AckTarget != "" {
		res, err := generic.FromProvider(deps, cfg.AckTarget)
		if err != nil {
			return nil, fmt.Errorf("cannot resolve ack_target %q: %w", cfg.AckTarget, err)
		}
		r.ackTarget = res
	}

	return r, nil
}

// DoCommand accepts:
//
//	{"transcript": "<text>"}  or  {"input": "<text>"}  → match and dispatch
//	{"list-phrases": true}                             → return the phrase table
//	{"test": "<text>"}                                 → dry-run match (no dispatch)
func (r *router) DoCommand(ctx context.Context, cmd map[string]interface{}) (map[string]interface{}, error) {
	if list, ok := cmd["list-phrases"].(bool); ok && list {
		return r.doList(), nil
	}
	if text, ok := cmd["test"].(string); ok {
		return r.doTest(text), nil
	}
	if text, ok := cmd["transcript"].(string); ok {
		return r.doInput(ctx, text)
	}
	if text, ok := cmd["input"].(string); ok {
		return r.doInput(ctx, text)
	}
	return nil, fmt.Errorf("router: expected one of {transcript, input, test, list-phrases} in command")
}

func (r *router) doInput(ctx context.Context, text string) (map[string]interface{}, error) {
	norm := normalize(text)
	if norm == "" {
		return map[string]interface{}{"handled": false, "reason": "empty transcript"}, nil
	}

	r.mu.RLock()
	routes := r.routes
	targets := r.targets
	fallback := r.fallback
	ackTarget := r.ackTarget
	r.mu.RUnlock()

	// Exact match against the whole normalized transcript.
	for _, route := range routes {
		if norm == route.say {
			r.logger.Infof("router: matched %q → target=%s", route.say, route.target)
			target, ok := targets[route.target]
			if !ok {
				return nil, fmt.Errorf("router: target %q not resolved", route.target)
			}
			actionResp, dispatchErr := target.DoCommand(ctx, route.do)
			if dispatchErr != nil {
				// Speak the failure message if configured, then return the
				// original error to the caller.
				spokenErr := ""
				if route.ackFailure != "" && ackTarget != nil {
					if _, ackErr := ackTarget.DoCommand(ctx, map[string]interface{}{"speak": route.ackFailure}); ackErr != nil {
						r.logger.Warnf("router: ack_failure speak failed: %v", ackErr)
					} else {
						spokenErr = route.ackFailure
					}
				}
				r.logger.Warnf("router: target %q DoCommand failed: %v (spoke: %q)", route.target, dispatchErr, spokenErr)
				return nil, fmt.Errorf("router: target %q DoCommand failed: %w", route.target, dispatchErr)
			}
			// Speak the acknowledgment, if configured. Errors here don't fail
			// the command — the action has already run.
			spoken := ""
			if route.ack != "" && ackTarget != nil {
				if _, ackErr := ackTarget.DoCommand(ctx, map[string]interface{}{"speak": route.ack}); ackErr != nil {
					r.logger.Warnf("router: ack speak failed: %v", ackErr)
				} else {
					spoken = route.ack
				}
			}
			return map[string]interface{}{
				"handled":  true,
				"matched":  route.say,
				"target":   route.target,
				"action":   route.do,
				"response": actionResp,
				"spoken":   spoken,
			}, nil
		}
	}

	// No match — fall through to LLM if configured.
	if fallback != nil {
		r.logger.Infof("router: no phrase match, forwarding to fallback: %q", text)
		actionResp, err := fallback.DoCommand(ctx, map[string]interface{}{"process": text})
		if err != nil {
			return nil, fmt.Errorf("router: fallback DoCommand failed: %w", err)
		}
		return map[string]interface{}{
			"handled":  true,
			"matched":  "",
			"target":   "<fallback>",
			"response": actionResp,
		}, nil
	}

	r.logger.Infof("router: no match for %q (no fallback configured)", text)
	return map[string]interface{}{"handled": false, "reason": "no phrase matched and no fallback configured"}, nil
}

func (r *router) doList() map[string]interface{} {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]map[string]interface{}, 0, len(r.routes))
	for _, route := range r.routes {
		entry := map[string]interface{}{
			"say":    route.say,
			"target": route.target,
			"do":     route.do,
		}
		if route.ack != "" {
			entry["ack"] = route.ack
		}
		if route.ackFailure != "" {
			entry["ack_failure"] = route.ackFailure
		}
		out = append(out, entry)
	}
	return map[string]interface{}{"routes": out, "ack_configured": r.ackTarget != nil}
}

func (r *router) doTest(text string) map[string]interface{} {
	norm := normalize(text)
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, route := range r.routes {
		if norm == route.say {
			resp := map[string]interface{}{
				"would_match": true,
				"matched":     route.say,
				"target":      route.target,
				"action":      route.do,
			}
			if route.ack != "" {
				resp["would_speak"] = route.ack
			}
			if route.ackFailure != "" {
				resp["would_speak_on_failure"] = route.ackFailure
			}
			return resp
		}
	}
	return map[string]interface{}{"would_match": false, "normalized": norm}
}

// normalize lowercases and trims the transcript for matching. Intentionally
// conservative — no punctuation stripping or fuzzy matching yet.
func normalize(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

func (r *router) Close(ctx context.Context) error {
	return nil
}
