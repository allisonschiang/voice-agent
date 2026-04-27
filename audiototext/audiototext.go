// Package audiototext implements the first stage of the voice pipeline:
// it owns the mic, forwards audio to Google Cloud Speech-to-Text, trims the
// wake word from transcripts, and hands each transcript off to a configured
// downstream generic service via DoCommand.
//
// Extracted from the original audio-to-model/recorder module, split so that
// downstream consumers (e.g. a text-router) can decide what to do with each
// transcript without this module needing to know.
package audiototext

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	speech "cloud.google.com/go/speech/apiv1"
	speechpb "cloud.google.com/go/speech/apiv1/speechpb"
	"google.golang.org/api/option"

	"go.viam.com/rdk/components/audioin"
	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/resource"
	generic "go.viam.com/rdk/services/generic"
	"go.viam.com/rdk/utils"
)

var Model = resource.NewModel("allisonorg", "voice-agent", "audio-to-text")

const (
	testRecordDuration = 5 * time.Second
	maxRecordDuration  = 40 * time.Second
)

// Config describes the inputs for audio-to-text.
type Config struct {
	AudioInput        string      `json:"audio_input"`
	GoogleCredentials interface{} `json:"google_credentials,omitempty"`
	WakeWord          string      `json:"wake_word,omitempty"`

	// Downstream is the name of a generic service that receives each
	// finalized transcript as {"transcript": "<text>"}. Typically a
	// text-router. Optional — if empty, transcripts are logged only.
	Downstream string `json:"downstream,omitempty"`

	Test bool `json:"test,omitempty"`
}

// Validate enforces required fields and returns dependencies.
func (c *Config) Validate(path string) ([]string, []string, error) {
	if c.AudioInput == "" {
		return nil, nil, fmt.Errorf(`%s: expected "audio_input" attribute`, path)
	}
	deps := []string{c.AudioInput}
	if c.Downstream != "" {
		deps = append(deps, "rdk:service:generic/"+c.Downstream)
	}
	return deps, nil, nil
}

func init() {
	resource.RegisterService(generic.API, Model, resource.Registration[resource.Resource, *Config]{
		Constructor: newAudioToText,
	})
}

type audioToText struct {
	resource.Named
	logger logging.Logger

	mu         sync.Mutex
	audioIn    audioin.AudioIn
	downstream resource.Resource
	test       bool

	recording bool
	cancelRec context.CancelFunc

	listening    bool
	cancelListen context.CancelFunc

	lastRecording []byte
	lastAudioInfo *utils.AudioInfo

	speechClient   *speech.Client
	lastTranscript string
	wakeWord       string
}

func newAudioToText(
	ctx context.Context,
	deps resource.Dependencies,
	conf resource.Config,
	logger logging.Logger,
) (resource.Resource, error) {
	a := &audioToText{
		Named:  conf.ResourceName().AsNamed(),
		logger: logger,
	}
	if err := a.Reconfigure(ctx, deps, conf); err != nil {
		return nil, err
	}
	return a, nil
}

// Reconfigure (re)wires dependencies and rebuilds the Google Speech client.
func (a *audioToText) Reconfigure(ctx context.Context, deps resource.Dependencies, conf resource.Config) error {
	cfg, err := resource.NativeConfig[*Config](conf)
	if err != nil {
		return err
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	// Stop any in-progress activity before reconfiguring.
	if a.cancelListen != nil {
		a.cancelListen()
		a.cancelListen = nil
		a.listening = false
	}
	if a.cancelRec != nil {
		a.cancelRec()
		a.cancelRec = nil
		a.recording = false
	}

	a.test = cfg.Test
	a.wakeWord = strings.ToLower(cfg.WakeWord)

	a.audioIn, err = audioin.FromProvider(deps, cfg.AudioInput)
	if err != nil {
		return fmt.Errorf("could not get audio input %q: %w", cfg.AudioInput, err)
	}

	if cfg.Downstream != "" {
		a.downstream, err = generic.FromProvider(deps, cfg.Downstream)
		if err != nil {
			return fmt.Errorf("could not get downstream %q: %w", cfg.Downstream, err)
		}
	} else {
		a.downstream = nil
	}

	// (Re)create the Google Speech client.
	if a.speechClient != nil {
		a.speechClient.Close()
		a.speechClient = nil
	}
	var opts []option.ClientOption
	if cfg.GoogleCredentials != nil {
		switch v := cfg.GoogleCredentials.(type) {
		case string:
			if strings.HasPrefix(strings.TrimSpace(v), "{") {
				opts = append(opts, option.WithCredentialsJSON([]byte(v)))
			} else {
				opts = append(opts, option.WithCredentialsFile(v))
			}
		default:
			credJSON, err := json.Marshal(v)
			if err != nil {
				return fmt.Errorf("failed to marshal google_credentials: %w", err)
			}
			opts = append(opts, option.WithCredentialsJSON(credJSON))
		}
	}
	a.speechClient, err = speech.NewClient(ctx, opts...)
	if err != nil {
		return fmt.Errorf("failed to create speech client: %w", err)
	}

	return nil
}

// DoCommand surface:
//
//	{"command":"listen"}  start continuous listen loop (wake-word gated by audio_input)
//	{"command":"stop"}    stop whatever is running (listen or record)
//	{"command":"record"}  single-shot record
//	{"command":"text"}    return the last transcript
func (a *audioToText) DoCommand(ctx context.Context, cmd map[string]interface{}) (map[string]interface{}, error) {
	a.logger.Infof("ALLISONDEBUGGING audio-to-text DoCommand received: %+v", cmd)
	command, ok := cmd["command"].(string)
	if !ok {
		return nil, fmt.Errorf("missing or invalid \"command\" field")
	}
	switch command {
	case "listen":
		return a.doListen(ctx)
	case "stop":
		return a.doStop()
	case "record":
		return a.doRecord(ctx)
	case "text":
		return a.doText()
	default:
		return nil, fmt.Errorf("unknown command %q", command)
	}
}

func (a *audioToText) doListen(ctx context.Context) (map[string]interface{}, error) {
	a.mu.Lock()
	if a.listening {
		a.mu.Unlock()
		return nil, fmt.Errorf("already listening")
	}
	if a.recording {
		a.mu.Unlock()
		return nil, fmt.Errorf("cannot listen while recording")
	}
	a.listening = true
	a.mu.Unlock()

	listenCtx, cancel := context.WithCancel(context.Background())

	a.mu.Lock()
	a.cancelListen = cancel
	a.mu.Unlock()

	go a.listenLoop(listenCtx)
	return map[string]interface{}{"status": "ok"}, nil
}

func (a *audioToText) listenLoop(ctx context.Context) {
	defer func() {
		a.mu.Lock()
		a.listening = false
		a.cancelListen = nil
		a.mu.Unlock()
		a.logger.Infof("audio-to-text: listen mode stopped")
	}()

	a.mu.Lock()
	audioIn := a.audioIn
	a.mu.Unlock()

	audioChan, err := audioIn.GetAudio(ctx, "pcm16", 0, 0, nil)
	if err != nil {
		a.logger.Errorf("ALLISONDEBUGGING audio-to-text: failed to start audio stream: %v", err)
		return
	}

	a.logger.Infof("ALLISONDEBUGGING audio-to-text: listen loop running — waiting for speech")

	for {
		transcript, ok := a.streamingCollectAndTranscribe(ctx, audioChan)
		if !ok {
			a.logger.Infof("ALLISONDEBUGGING audio-to-text: listen loop exiting (context cancelled or stream closed)")
			return
		}
		a.logger.Infof("ALLISONDEBUGGING audio-to-text: raw transcript from STT (before wake-word trim): %q", transcript)
		if transcript == "" {
			a.logger.Infof("ALLISONDEBUGGING audio-to-text: empty transcript, skipping")
			continue
		}

		trimmed := a.trimWakeWord(transcript)
		a.logger.Infof("ALLISONDEBUGGING audio-to-text: transcript after wake-word trim: %q (wake_word=%q)", trimmed, a.wakeWord)
		if trimmed == "" {
			a.logger.Infof("ALLISONDEBUGGING audio-to-text: transcript empty after trim, skipping")
			continue
		}
		transcript = trimmed

		a.mu.Lock()
		a.lastTranscript = transcript
		downstream := a.downstream
		a.mu.Unlock()

		if downstream != nil {
			payload := map[string]interface{}{"transcript": transcript}
			a.logger.Infof("ALLISONDEBUGGING audio-to-text: dispatching to downstream with payload: %+v", payload)
			resp, err := downstream.DoCommand(ctx, payload)
			if err != nil {
				a.logger.Errorf("ALLISONDEBUGGING audio-to-text: downstream DoCommand FAILED: %v", err)
			} else {
				a.logger.Infof("ALLISONDEBUGGING audio-to-text: downstream response: %+v", resp)
			}
		} else {
			a.logger.Infof("ALLISONDEBUGGING audio-to-text: no downstream configured — transcript logged only")
		}
		if ctx.Err() != nil {
			return
		}
	}
}

func (a *audioToText) trimWakeWord(transcript string) string {
	a.mu.Lock()
	ww := a.wakeWord
	a.mu.Unlock()
	if ww == "" {
		return transcript
	}
	lower := strings.ToLower(transcript)
	if idx := strings.Index(lower, ww); idx >= 0 {
		return strings.TrimSpace(transcript[idx+len(ww):])
	}
	return transcript
}

// streamingCollectAndTranscribe reads audio from audioChan and streams it to
// Google STT in real time, returning the final transcript at the utterance
// boundary (signaled by an empty chunk from the wake-word filter).
func (a *audioToText) streamingCollectAndTranscribe(ctx context.Context, audioChan chan *audioin.AudioChunk) (string, bool) {
	a.mu.Lock()
	client := a.speechClient
	a.mu.Unlock()

	if client == nil {
		// Fall back to non-streaming collect → transcribe.
		buf, info, ok := a.collectUtterance(ctx, audioChan)
		if !ok {
			return "", false
		}
		a.mu.Lock()
		a.lastRecording = buf
		a.lastAudioInfo = info
		a.mu.Unlock()
		a.transcribe(ctx)
		a.mu.Lock()
		t := a.lastTranscript
		a.mu.Unlock()
		return t, true
	}

	streamCtx, streamCancel := context.WithTimeout(ctx, 60*time.Second)
	defer streamCancel()

	a.logger.Infof("ALLISONDEBUGGING audio-to-text: opening new Google STT streaming connection (timeout=60s)")
	stream, err := client.StreamingRecognize(streamCtx)
	if err != nil {
		a.logger.Errorf("ALLISONDEBUGGING audio-to-text: StreamingRecognize open failed: %v", err)
		return "", true
	}

	if err := stream.Send(&speechpb.StreamingRecognizeRequest{
		StreamingRequest: &speechpb.StreamingRecognizeRequest_StreamingConfig{
			StreamingConfig: &speechpb.StreamingRecognitionConfig{
				Config: &speechpb.RecognitionConfig{
					Encoding:        speechpb.RecognitionConfig_LINEAR16,
					SampleRateHertz: 16000,
					LanguageCode:    "en-US",
				},
			},
		},
	}); err != nil {
		a.logger.Errorf("audio-to-text: send streaming config failed: %v", err)
		return "", true
	}

	type sttResult struct {
		transcript string
		err        error
	}
	resultCh := make(chan sttResult, 1)
	go func() {
		var finalTranscript string
		for {
			resp, err := stream.Recv()
			if err == io.EOF {
				break
			}
			if err != nil {
				resultCh <- sttResult{err: err}
				return
			}
			for _, result := range resp.Results {
				if result.IsFinal && len(result.Alternatives) > 0 {
					finalTranscript += result.Alternatives[0].Transcript + " "
				}
			}
		}
		resultCh <- sttResult{transcript: strings.TrimSpace(finalTranscript)}
	}()

	var buf []byte
	chunkCount := 0
	firstChunkLogged := false
	for {
		select {
		case <-ctx.Done():
			a.logger.Infof("ALLISONDEBUGGING audio-to-text: context cancelled mid-stream after %d chunks, %d bytes", chunkCount, len(buf))
			stream.CloseSend()
			return "", false
		case chunk, ok := <-audioChan:
			if !ok {
				a.logger.Infof("ALLISONDEBUGGING audio-to-text: audioChan closed after %d chunks, %d bytes", chunkCount, len(buf))
				stream.CloseSend()
				return "", false
			}
			if len(chunk.AudioData) == 0 {
				a.logger.Infof("ALLISONDEBUGGING audio-to-text: utterance boundary (empty chunk) received after %d chunks, %d bytes — closing stream", chunkCount, len(buf))
				stream.CloseSend()
				goto waitResult
			}
			if !firstChunkLogged {
				a.logger.Infof("ALLISONDEBUGGING audio-to-text: first audio chunk received (%d bytes) — speech detected upstream", len(chunk.AudioData))
				firstChunkLogged = true
			}
			chunkCount++
			buf = append(buf, chunk.AudioData...)
			if err := stream.Send(&speechpb.StreamingRecognizeRequest{
				StreamingRequest: &speechpb.StreamingRecognizeRequest_AudioContent{
					AudioContent: chunk.AudioData,
				},
			}); err != nil {
				a.logger.Warnf("ALLISONDEBUGGING audio-to-text: send audio chunk failed after %d chunks, %d bytes: %v", chunkCount, len(buf), err)
				stream.CloseSend()
				goto waitResult
			}
		}
	}

waitResult:
	a.logger.Infof("ALLISONDEBUGGING audio-to-text: streaming STT sent %d bytes across %d chunks, waiting for final transcript", len(buf), chunkCount)

	a.mu.Lock()
	a.lastRecording = buf
	a.mu.Unlock()

	select {
	case res := <-resultCh:
		if res.err != nil {
			a.logger.Errorf("ALLISONDEBUGGING audio-to-text: streaming STT error: %v", res.err)
			return "", true
		}
		a.logger.Infof("ALLISONDEBUGGING audio-to-text: STT returned final transcript: %q", res.transcript)
		return res.transcript, true
	case <-time.After(10 * time.Second):
		a.logger.Errorf("ALLISONDEBUGGING audio-to-text: streaming STT timed out waiting for final result (10s after CloseSend)")
		return "", true
	case <-ctx.Done():
		a.logger.Infof("ALLISONDEBUGGING audio-to-text: context cancelled while waiting for STT final")
		return "", false
	}
}

func (a *audioToText) collectUtterance(ctx context.Context, audioChan chan *audioin.AudioChunk) ([]byte, *utils.AudioInfo, bool) {
	var buf []byte
	var info *utils.AudioInfo
	for {
		select {
		case <-ctx.Done():
			return nil, nil, false
		case chunk, ok := <-audioChan:
			if !ok {
				return nil, nil, false
			}
			if len(chunk.AudioData) == 0 {
				return buf, info, true
			}
			if info == nil && chunk.AudioInfo != nil {
				info = chunk.AudioInfo
			}
			buf = append(buf, chunk.AudioData...)
		}
	}
}

func (a *audioToText) transcribe(pipelineCtx context.Context) {
	a.mu.Lock()
	data := a.lastRecording
	info := a.lastAudioInfo
	client := a.speechClient
	a.mu.Unlock()

	if len(data) == 0 || client == nil {
		return
	}

	ctx, cancel := context.WithTimeout(pipelineCtx, 30*time.Second)
	defer cancel()

	sampleRate := int32(16000)
	if info != nil && info.SampleRateHz > 0 {
		sampleRate = info.SampleRateHz
	}

	resp, err := client.Recognize(ctx, &speechpb.RecognizeRequest{
		Config: &speechpb.RecognitionConfig{
			Encoding:        speechpb.RecognitionConfig_LINEAR16,
			SampleRateHertz: sampleRate,
			LanguageCode:    "en-US",
		},
		Audio: &speechpb.RecognitionAudio{
			AudioSource: &speechpb.RecognitionAudio_Content{
				Content: data,
			},
		},
	})
	if err != nil {
		a.logger.Errorf("audio-to-text: transcription failed: %v", err)
		return
	}

	var parts []string
	for _, result := range resp.Results {
		if len(result.Alternatives) > 0 {
			parts = append(parts, result.Alternatives[0].Transcript)
		}
	}
	transcript := a.trimWakeWord(strings.Join(parts, " "))

	a.mu.Lock()
	a.lastTranscript = transcript
	a.mu.Unlock()

	a.logger.Infof("audio-to-text: transcript: %s", transcript)
}

func (a *audioToText) doRecord(ctx context.Context) (map[string]interface{}, error) {
	a.mu.Lock()
	if a.recording {
		a.mu.Unlock()
		return nil, fmt.Errorf("already recording")
	}
	if a.listening {
		a.mu.Unlock()
		return nil, fmt.Errorf("cannot record while listening")
	}
	a.lastTranscript = ""
	a.recording = true
	a.mu.Unlock()

	recCtx, cancel := context.WithCancel(context.Background())
	audioChan, err := a.audioIn.GetAudio(recCtx, "pcm16", 0, 0, nil)
	if err != nil {
		cancel()
		a.mu.Lock()
		a.recording = false
		a.mu.Unlock()
		return nil, fmt.Errorf("failed to start audio stream: %w", err)
	}

	a.mu.Lock()
	a.cancelRec = cancel
	a.mu.Unlock()

	go a.recordLoop(recCtx, cancel, audioChan)
	return map[string]interface{}{"status": "ok"}, nil
}

func (a *audioToText) recordLoop(ctx context.Context, cancel context.CancelFunc, audioChan chan *audioin.AudioChunk) {
	a.mu.Lock()
	testMode := a.test
	a.mu.Unlock()

	dur := maxRecordDuration
	if testMode {
		dur = testRecordDuration
	}
	timer := time.NewTimer(dur)
	defer timer.Stop()

	var buf []byte
	var info *utils.AudioInfo

	done := false
	for !done {
		select {
		case chunk, ok := <-audioChan:
			if !ok {
				done = true
				break
			}
			if info == nil && chunk.AudioInfo != nil {
				info = chunk.AudioInfo
			}
			buf = append(buf, chunk.AudioData...)
		case <-timer.C:
			cancel()
		case <-ctx.Done():
			for chunk := range audioChan {
				if info == nil && chunk.AudioInfo != nil {
					info = chunk.AudioInfo
				}
				buf = append(buf, chunk.AudioData...)
			}
			done = true
		}
	}

	a.mu.Lock()
	a.lastRecording = buf
	a.lastAudioInfo = info
	a.recording = false
	a.cancelRec = nil
	a.mu.Unlock()

	a.logger.Infof("audio-to-text: recording finished: %d bytes", len(buf))

	a.transcribe(context.Background())

	a.mu.Lock()
	transcript := a.lastTranscript
	downstream := a.downstream
	a.mu.Unlock()

	if transcript != "" && downstream != nil {
		if _, err := downstream.DoCommand(context.Background(), map[string]interface{}{"transcript": transcript}); err != nil {
			a.logger.Errorf("audio-to-text: downstream DoCommand failed: %v", err)
		}
	}
}

func (a *audioToText) doStop() (map[string]interface{}, error) {
	a.mu.Lock()
	if a.listening && a.cancelListen != nil {
		cancel := a.cancelListen
		a.mu.Unlock()
		cancel()
		return map[string]interface{}{"status": "ok", "stopped": "listening"}, nil
	}
	if !a.recording {
		a.mu.Unlock()
		return nil, fmt.Errorf("not recording or listening")
	}
	cancel := a.cancelRec
	a.mu.Unlock()
	cancel()

	for i := 0; i < 50; i++ {
		time.Sleep(10 * time.Millisecond)
		a.mu.Lock()
		if !a.recording {
			n := len(a.lastRecording)
			a.mu.Unlock()
			return map[string]interface{}{"status": "ok", "bytes": n}, nil
		}
		a.mu.Unlock()
	}
	return map[string]interface{}{"status": "ok", "bytes": 0}, nil
}

func (a *audioToText) doText() (map[string]interface{}, error) {
	a.mu.Lock()
	t := a.lastTranscript
	a.mu.Unlock()
	if t == "" {
		return nil, fmt.Errorf("no transcript available")
	}
	return map[string]interface{}{"status": "ok", "text": t}, nil
}

func (a *audioToText) Close(ctx context.Context) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.cancelListen != nil {
		a.cancelListen()
		a.cancelListen = nil
		a.listening = false
	}
	if a.cancelRec != nil {
		a.cancelRec()
		a.cancelRec = nil
		a.recording = false
	}
	if a.speechClient != nil {
		a.speechClient.Close()
	}
	return nil
}
