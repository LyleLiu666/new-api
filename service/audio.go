package service

import (
	"encoding/base64"
	"fmt"
	"strings"
)

// The duration and its parameters are taken from the same decoded payload.
// Unknown formats retain the native fallback, explicitly identified as such.
func parseAudioObservation(audioBase64 string, format string) (float64, map[string]float64, error) {
	audioData, err := base64.StdEncoding.DecodeString(audioBase64)
	if err != nil {
		return 0, nil, fmt.Errorf("base64 decode error: %v", err)
	}

	var samplesCount int
	var sampleRate int
	bytesPerSample := 1

	switch format {
	case "pcm16":
		samplesCount = len(audioData) / 2 // 16位 = 2字节每样本
		sampleRate = 24000                // 24kHz
		bytesPerSample = 2
	case "g711_ulaw", "g711_alaw":
		samplesCount = len(audioData) // 8位 = 1字节每样本
		sampleRate = 8000             // 8kHz
	default:
		samplesCount = len(audioData) // 8位 = 1字节每样本
		sampleRate = 8000             // 8kHz
	}

	duration := float64(samplesCount) / float64(sampleRate)
	return duration, map[string]float64{"decoded_bytes": float64(len(audioData)), "samples": float64(samplesCount), "sample_rate": float64(sampleRate), "bytes_per_sample": float64(bytesPerSample), "duration_seconds": duration}, nil
}

func DecodeBase64AudioData(audioBase64 string) (string, error) {
	// 检查并移除 data:audio/xxx;base64, 前缀
	idx := strings.Index(audioBase64, ",")
	if idx != -1 {
		audioBase64 = audioBase64[idx+1:]
	}

	// 解码 Base64 数据
	_, err := base64.StdEncoding.DecodeString(audioBase64)
	if err != nil {
		return "", fmt.Errorf("base64 decode error: %v", err)
	}

	return audioBase64, nil
}
