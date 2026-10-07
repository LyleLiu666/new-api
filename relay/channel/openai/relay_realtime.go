package openai

import (
	"errors"
	"fmt"
	"sync"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/logger"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
)

type realtimeFrame struct {
	client  bool
	message []byte
	err     error
}

var errInvalidRealtimeUsage = errors.New("invalid or excessive realtime usage")

// Readers own only transport. The relay loop owns every usage counter and
// mutable relay field, including shutdown accounting.
func readRealtimeFrames(conn *websocket.Conn, client bool, frames chan<- realtimeFrame, stop <-chan struct{}) {
	defer func() {
		if value := recover(); value != nil {
			select {
			case frames <- realtimeFrame{client: client, err: fmt.Errorf("realtime reader panic: %v", value)}:
			case <-stop:
			}
		}
	}()
	for {
		_, message, err := conn.ReadMessage()
		select {
		case frames <- realtimeFrame{client: client, message: message, err: err}:
		case <-stop:
			return
		}
		if err != nil {
			return
		}
	}
}

func OpenaiRealtimeHandler(c *gin.Context, info *relaycommon.RelayInfo) (*types.NewAPIError, *dto.RealtimeUsage) {
	if info == nil || info.ClientWs == nil || info.TargetWs == nil {
		return types.NewError(fmt.Errorf("invalid websocket connection"), types.ErrorCodeBadResponse), nil
	}
	info.IsStream = true
	frames := make(chan realtimeFrame, 2)
	stop := make(chan struct{})
	var readers sync.WaitGroup
	readers.Go(func() { readRealtimeFrames(info.ClientWs, true, frames, stop) })
	readers.Go(func() { readRealtimeFrames(info.TargetWs, false, frames, stop) })
	defer func() { close(stop); _ = info.ClientWs.Close(); _ = info.TargetWs.Close(); readers.Wait() }()
	localUsage, sumUsage := &dto.RealtimeUsage{}, &dto.RealtimeUsage{}
	seen := make(map[string]struct{})
	credit := service.CreditBillingRequestID(info) != 0
	var terminalErr error
	knownUsage := false
	for terminalErr == nil {
		var frame realtimeFrame
		select {
		case frame = <-frames:
		case <-c.Request.Context().Done():
			terminalErr = c.Request.Context().Err()
			continue
		}
		if frame.err != nil {
			if !frame.client && websocket.IsCloseError(frame.err, websocket.CloseNormalClosure, websocket.CloseGoingAway) && localUsage.TotalTokens == 0 {
				knownUsage = true
			}
			terminalErr = frame.err
			continue
		}
		var event dto.RealtimeEvent
		if err := common.Unmarshal(frame.message, &event); err != nil {
			terminalErr = err
			continue
		}
		if frame.client {
			if event.Type == dto.RealtimeEventTypeSessionUpdate && event.Session != nil && event.Session.Tools != nil {
				info.RealtimeTools = event.Session.Tools
			}
			text, audio, err := service.CountTokenRealtime(info, event, info.UpstreamModelName)
			if err != nil {
				terminalErr = err
				continue
			}
			terminalErr = addRealtimeUsage(localUsage, &dto.RealtimeUsage{TotalTokens: text + audio, InputTokens: text + audio, InputTokenDetails: dto.InputTokenDetails{TextTokens: text, AudioTokens: audio}})
			if terminalErr != nil {
				continue
			}
			terminalErr = helper.WssString(c, info.TargetWs, string(frame.message))
			continue
		}
		info.SetFirstResponseTime()
		switch event.Type {
		case dto.RealtimeEventTypeResponseDone:
			if _, repeated := seen[event.EventId]; event.EventId != "" && repeated {
				break
			}
			if event.EventId != "" {
				seen[event.EventId] = struct{}{}
			}
			if event.Response == nil {
				terminalErr = fmt.Errorf("realtime completion has no response")
				continue
			}
			usage := event.Response.Usage
			if usage == nil {
				if credit {
					terminalErr = fmt.Errorf("realtime completion has unknown usage")
					continue
				}
				text, audio, err := service.CountTokenRealtime(info, event, info.UpstreamModelName)
				if err != nil {
					terminalErr = err
					continue
				}
				info.IsFirstRequest = false
				if err := addRealtimeUsage(localUsage, &dto.RealtimeUsage{TotalTokens: text + audio, InputTokens: text + audio, InputTokenDetails: dto.InputTokenDetails{TextTokens: text, AudioTokens: audio}}); err != nil {
					terminalErr = err
					continue
				}
				usage = localUsage
			}
			if err := preConsumeUsage(c, info, usage, sumUsage); err != nil {
				// A valid receipt describes work already done. Stop further upstream
				// work and settle the known total with the accepted debt rule.
				terminalErr = err
				knownUsage = credit && !errors.Is(err, errInvalidRealtimeUsage)
				continue
			}
			localUsage = &dto.RealtimeUsage{}
		case dto.RealtimeEventTypeSessionUpdated, dto.RealtimeEventTypeSessionCreated:
			if event.Session != nil {
				info.InputAudioFormat = common.GetStringIfEmpty(event.Session.InputAudioFormat, info.InputAudioFormat)
				info.OutputAudioFormat = common.GetStringIfEmpty(event.Session.OutputAudioFormat, info.OutputAudioFormat)
			}
		default:
			text, audio, err := service.CountTokenRealtime(info, event, info.UpstreamModelName)
			if err != nil {
				terminalErr = err
				continue
			}
			terminalErr = addRealtimeUsage(localUsage, &dto.RealtimeUsage{TotalTokens: text + audio, OutputTokens: text + audio, OutputTokenDetails: dto.OutputTokenDetails{TextTokens: text, AudioTokens: audio}})
			if terminalErr != nil {
				continue
			}
		}
		terminalErr = helper.WssString(c, info.ClientWs, string(frame.message))
	}
	if credit && !knownUsage {
		return types.NewError(terminalErr, types.ErrorCodeBadResponse, types.ErrOptionWithSkipRetry()), nil
	}
	if !credit && localUsage.TotalTokens != 0 {
		_ = preConsumeUsage(c, info, localUsage, sumUsage)
	}
	logger.LogDebug(c, "realtime relay ended: %v", terminalErr)
	return nil, sumUsage
}

// Validate all provider fields before mutating any accumulator. Oversized
// counts cannot wrap into a smaller bill.
func addRealtimeUsage(total, usage *dto.RealtimeUsage) error {
	if total == nil || usage == nil {
		return fmt.Errorf("invalid realtime usage")
	}
	fields := []struct {
		target *int
		delta  int
	}{
		{&total.TotalTokens, usage.TotalTokens}, {&total.InputTokens, usage.InputTokens}, {&total.OutputTokens, usage.OutputTokens},
		{&total.InputTokenDetails.CachedTokens, usage.InputTokenDetails.CachedTokens}, {&total.InputTokenDetails.TextTokens, usage.InputTokenDetails.TextTokens}, {&total.InputTokenDetails.AudioTokens, usage.InputTokenDetails.AudioTokens},
		{&total.OutputTokenDetails.TextTokens, usage.OutputTokenDetails.TextTokens}, {&total.OutputTokenDetails.AudioTokens, usage.OutputTokenDetails.AudioTokens},
	}
	for _, field := range fields {
		if field.delta < 0 || field.delta > common.MaxQuota || *field.target < 0 || *field.target > common.MaxQuota-field.delta {
			return errInvalidRealtimeUsage
		}
	}
	for _, field := range fields {
		*field.target += field.delta
	}
	return nil
}

func preConsumeUsage(ctx *gin.Context, info *relaycommon.RelayInfo, usage, totalUsage *dto.RealtimeUsage) error {
	if err := addRealtimeUsage(totalUsage, usage); err != nil {
		return err
	}
	if service.CreditBillingRequestID(info) != 0 {
		return service.PreWssConsumeQuota(ctx, info, totalUsage)
	}
	return service.PreWssConsumeQuota(ctx, info, usage)
}
