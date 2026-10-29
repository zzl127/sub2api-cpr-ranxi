// Package rawproxy forwards API traffic without business-protocol conversion.
// Metering reads a bounded side copy; it never serializes forwarded payloads.
package rawproxy

import (
	"bufio"
	"bytes"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const maxObservationBytes = 4 << 20

type Usage struct {
	Input, Output, CacheRead, CacheCreation int
	ImageInput, ImageOutput, ImageCacheRead int
}

type Sample struct {
	Terminal                                                       bool
	TerminalEvent                                                  string
	ResponseID, Model, ServiceTier, Event                          string
	RequestedModel, RequestedServiceTier, RequestedReasoningEffort string
	UpstreamHeaders                                                http.Header
	Usage                                                          Usage
	Found                                                          bool
	ObservationLimited                                             bool
	ImageCount                                                     int
	FirstToken                                                     *time.Duration
}

// Meter retains cumulative usage, rather than adding duplicate SSE snapshots.
type Meter struct {
	Sample
	started          time.Time
	anthropicIn      int
	anthropicSeen    bool
	inputSeen        bool
	outputSeen       bool
	startsFirstToken func([]byte) bool
	ObservePayload   func([]byte)
}

func NewMeter() *Meter { return NewMeterAt(time.Now(), nil) }

func NewMeterAt(started time.Time, predicate func([]byte) bool) *Meter {
	if started.IsZero() {
		started = time.Now()
	}
	if predicate == nil {
		predicate = DefaultStartsFirstToken
	}
	return &Meter{started: started, startsFirstToken: predicate}
}

// Protocol-neutral fallback. The gateway supplies its configured Responses
// semantic/visible classifier without coupling this wire package to services.
func DefaultStartsFirstToken(data []byte) bool {
	root := gjson.ParseBytes(data)
	if root.Get("delta").Type == gjson.String && root.Get("delta").String() != "" {
		return true
	}
	for _, path := range []string{"delta.text", "delta.thinking", "delta.partial_json",
		"choices.0.delta.content", "choices.0.delta.reasoning_content",
		"choices.0.delta.tool_calls.0.function.arguments", "choices.0.delta.function_call.arguments"} {
		if root.Get(path).String() != "" {
			return true
		}
	}
	if root.Get("type").String() == "content_block_start" {
		block := root.Get("content_block")
		return block.Get("type").String() == "tool_use" || block.Get("text").String() != ""
	}
	return false
}

func nonnegative(v gjson.Result) (int, bool) {
	if !v.Exists() || v.Type != gjson.Number {
		return 0, false
	}
	n := v.Int()
	if n < 0 || n > 1<<52 || v.Float() != float64(n) {
		return 0, false
	}
	return int(n), true
}

func (m *Meter) Observe(data []byte) {
	if len(data) > maxObservationBytes {
		m.ObservationLimited = true
		return
	}
	if !gjson.ValidBytes(data) {
		return
	}
	if m.ObservePayload != nil {
		m.ObservePayload(data)
	}
	root := gjson.ParseBytes(data)
	event := root.Get("type").String()
	switch event {
	case "response.completed", "response.done", "response.failed", "response.incomplete", "error", "message_stop",
		"image_generation.completed", "image_edit.completed":
		m.Terminal, m.TerminalEvent = true, event
	}
	if root.Get("choices.0.finish_reason").String() != "" {
		m.Terminal, m.TerminalEvent = true, "chat.completed"
	}
	obj := root
	if r := root.Get("response"); r.IsObject() {
		obj = r
	} else if r := root.Get("message"); r.IsObject() {
		obj = r
	}
	if id := obj.Get("id").String(); id != "" {
		m.ResponseID = id
	}
	if model := obj.Get("model").String(); model != "" {
		m.Model = model
	}
	if tier := obj.Get("service_tier").String(); tier != "" {
		m.ServiceTier = tier
	}
	if event != "" {
		m.Event = event
	}
	if m.FirstToken == nil && m.startsFirstToken(data) {
		elapsed := time.Since(m.started)
		m.FirstToken = &elapsed
	}
	u := obj.Get("usage")
	if !u.IsObject() {
		u = root.Get("usage")
	}
	if !u.IsObject() {
		return
	}
	assign := func(dest *int, paths ...string) bool {
		for _, p := range paths {
			if n, ok := nonnegative(u.Get(p)); ok {
				*dest = n
				return true
			}
		}
		return false
	}
	isAnthropic := strings.HasPrefix(event, "message_") || obj.Get("type").String() == "message"
	if isAnthropic {
		m.anthropicSeen = true
		m.inputSeen = assign(&m.anthropicIn, "input_tokens") || m.inputSeen
	} else {
		m.inputSeen = assign(&m.Usage.Input, "input_tokens", "prompt_tokens") || m.inputSeen
	}
	m.outputSeen = assign(&m.Usage.Output, "output_tokens", "completion_tokens") || m.outputSeen
	m.Found = m.inputSeen && m.outputSeen
	assign(&m.Usage.CacheRead, "input_tokens_details.cached_tokens", "prompt_tokens_details.cached_tokens", "cache_read_input_tokens")
	assign(&m.Usage.CacheCreation, "cache_creation_input_tokens", "input_tokens_details.cache_creation_tokens", "input_tokens_details.cache_write_tokens")
	assign(&m.Usage.ImageInput, "input_tokens_details.image_tokens", "prompt_tokens_details.image_tokens")
	assign(&m.Usage.ImageOutput, "output_tokens_details.image_tokens", "completion_tokens_details.image_tokens")
	assign(&m.Usage.ImageCacheRead, "input_tokens_details.cached_tokens_details.image_tokens")
	imageUsage := obj.Get("tool_usage.image_gen")
	if !imageUsage.Exists() {
		imageUsage = root.Get("tool_usage.image_gen")
	}
	if m.Usage.ImageInput == 0 {
		if n, ok := nonnegative(imageUsage.Get("input_tokens_details.image_tokens")); ok {
			m.Usage.ImageInput = n
		}
	}
	if m.Usage.ImageOutput == 0 {
		if n, ok := nonnegative(imageUsage.Get("output_tokens_details.image_tokens")); ok {
			m.Usage.ImageOutput = n
		}
	}
	if m.anthropicSeen {
		m.Usage.Input = m.anthropicIn + m.Usage.CacheRead + m.Usage.CacheCreation
	}
}

func (m *Meter) observeEvent(data []byte, eventType string) {
	if bytes.Equal(bytes.TrimSpace(data), []byte("[DONE]")) {
		m.Terminal = true
		if m.TerminalEvent == "" {
			m.TerminalEvent = "[DONE]"
		}
		return
	}
	if eventType != "" && gjson.ValidBytes(data) && gjson.GetBytes(data, "type").String() == "" {
		// This is an observation copy. The original event is already on the wire.
		if typed, err := sjson.SetBytes(data, "type", eventType); err == nil {
			data = typed
		}
	}
	m.Observe(data)
}

// ObserveReader drains every byte even if usage is malformed or too large.
// The caller uses a tee reader to deliver the ORIGINAL wire bytes downstream.
func (m *Meter) ObserveReader(r io.Reader, sse bool) error {
	if !sse {
		b, err := io.ReadAll(io.LimitReader(r, maxObservationBytes+1))
		if err != nil {
			return err
		}
		if len(b) <= maxObservationBytes {
			m.Observe(b)
		} else {
			m.ObservationLimited = true
		}
		_, err = io.Copy(io.Discard, r)
		return err
	}
	reader := bufio.NewReaderSize(r, 32<<10)
	var line, event []byte
	eventType := ""
	dropLine, dropEvent := false, false
	for {
		part, err := reader.ReadSlice('\n')
		if !dropLine {
			if len(line)+len(part) > maxObservationBytes {
				line, dropLine = nil, true
				m.ObservationLimited = true
			} else {
				line = append(line, part...)
			}
		}
		if err == bufio.ErrBufferFull {
			continue
		}
		if dropLine {
			dropEvent = true
		} else {
			line = bytes.TrimRight(line, "\r\n")
			if len(line) == 0 {
				if !dropEvent && len(event) > 0 {
					m.observeEvent(bytes.TrimSuffix(event, []byte{'\n'}), eventType)
				}
				event, dropEvent = nil, false
				eventType = ""
			} else if bytes.HasPrefix(line, []byte("event:")) {
				if len(line) <= 256 {
					eventType = strings.TrimSpace(string(line[len("event:"):]))
				}
			} else if bytes.HasPrefix(line, []byte("data:")) {
				v := line[len("data:"):]
				if len(v) > 0 && v[0] == ' ' {
					v = v[1:]
				}
				if len(event)+len(v)+1 > maxObservationBytes {
					event, dropEvent = nil, true
					m.ObservationLimited = true
				} else if !dropEvent {
					event = append(append(event, v...), '\n')
				}
			}
		}
		line, dropLine = nil, false
		if err != nil {
			if err == io.EOF {
				if !dropEvent && len(event) > 0 {
					m.observeEvent(bytes.TrimSuffix(event, []byte{'\n'}), eventType)
				}
				return nil
			}
			return err
		}
	}
}
