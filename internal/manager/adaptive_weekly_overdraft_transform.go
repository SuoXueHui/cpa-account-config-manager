package manager

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"strings"
	"time"

	"cpa-account-config-manager/internal/cpaapi"
)

var (
	adaptiveOriginalCallMarker = []byte("call_cpa_overdraft_")
	adaptiveCallMarker         = []byte("call_cpa_adaptive_")
)

func (e *AdaptiveWeeklyOverdraftExperiment) RequestInterceptionActive() bool {
	if e == nil || e.enabled == nil || !e.enabled() {
		return false
	}
	e.mu.RLock()
	active := e.configured && e.hostSchema >= cpaapi.SchemaVersion && e.storageErr == ""
	e.mu.RUnlock()
	return active
}

func (e *AdaptiveWeeklyOverdraftExperiment) RequestInterceptionAcceptsFormat(format string) bool {
	return strings.EqualFold(strings.TrimSpace(format), "codex")
}

func (e *AdaptiveWeeklyOverdraftExperiment) InterceptRequest(request cpaapi.RequestInterceptRequest) (cpaapi.RequestInterceptResponse, bool) {
	if e == nil || !e.RequestInterceptionActive() || !e.RequestInterceptionAcceptsFormat(request.ToFormat) {
		return cpaapi.RequestInterceptResponse{}, false
	}
	authID, _ := request.Metadata[selectedAuthMetadataKey].(string)
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return cpaapi.RequestInterceptResponse{}, false
	}
	strategy, eligible := e.strategyForAuthID(authID, e.currentTime())
	if !eligible {
		return cpaapi.RequestInterceptResponse{}, false
	}
	response, shape, changed := e.interceptRequestForStrategy(request, strategy, true)
	if changed {
		e.recordRequestWithShape(request.RequestID, authID, strategy, shape, e.currentTime())
	}
	return response, changed
}

func (e *AdaptiveWeeklyOverdraftExperiment) InterceptRequestForStrategy(request cpaapi.RequestInterceptRequest, strategy AdaptiveOverdraftStrategy, requireEligibility bool) (cpaapi.RequestInterceptResponse, bool) {
	response, _, changed := e.interceptRequestForStrategy(request, strategy, requireEligibility)
	return response, changed
}

func (e *AdaptiveWeeklyOverdraftExperiment) interceptRequestForStrategy(request cpaapi.RequestInterceptRequest, strategy AdaptiveOverdraftStrategy, requireEligibility bool) (cpaapi.RequestInterceptResponse, AdaptiveRequestShape, bool) {
	if e == nil || !e.RequestInterceptionActive() || !e.RequestInterceptionAcceptsFormat(request.ToFormat) || adaptiveStrategyPairCount(strategy) == 0 {
		return cpaapi.RequestInterceptResponse{}, "", false
	}
	authID, _ := request.Metadata[selectedAuthMetadataKey].(string)
	authID = strings.TrimSpace(authID)
	if requireEligibility {
		selected, eligible := e.strategyForAuthID(authID, e.currentTime())
		if !eligible || selected != strategy {
			return cpaapi.RequestInterceptResponse{}, "", false
		}
	}
	bodyLimit := e.bodyLimit()
	if len(request.Body) == 0 || len(request.Body) > bodyLimit ||
		bytes.Contains(request.Body, adaptiveOriginalCallMarker) || bytes.Contains(request.Body, adaptiveCallMarker) ||
		!bytes.Contains(request.Body, weeklyOverdraftInputMarkerBytes) {
		return cpaapi.RequestInterceptResponse{}, "", false
	}
	var document struct {
		Input json.RawMessage `json:"input"`
	}
	if errDecode := json.Unmarshal(request.Body, &document); errDecode != nil || len(document.Input) == 0 {
		return cpaapi.RequestInterceptResponse{}, "", false
	}
	var input []json.RawMessage
	if errInput := json.Unmarshal(document.Input, &input); errInput != nil || len(input) == 0 {
		return cpaapi.RequestInterceptResponse{}, "", false
	}
	shape, accepted := e.classifyRequestShape(input[len(input)-1], authID)
	if !accepted {
		return cpaapi.RequestInterceptResponse{}, "", false
	}
	trimmedInput := bytes.TrimSpace(document.Input)
	if len(trimmedInput) < 2 || trimmedInput[0] != '[' || trimmedInput[len(trimmedInput)-1] != ']' {
		return cpaapi.RequestInterceptResponse{}, "", false
	}
	appended, ok := e.adaptiveToolPairs(strategy)
	if !ok {
		return cpaapi.RequestInterceptResponse{}, "", false
	}
	updatedInput := make([]byte, 0, len(trimmedInput)+len(appended)+1)
	updatedInput = append(updatedInput, trimmedInput[:len(trimmedInput)-1]...)
	updatedInput = append(updatedInput, ',')
	updatedInput = append(updatedInput, appended...)
	updatedInput = append(updatedInput, ']')
	updated, replaced := replaceTopLevelJSONFieldValue(request.Body, "input", document.Input, updatedInput)
	if !replaced || len(updated) > bodyLimit {
		return cpaapi.RequestInterceptResponse{}, "", false
	}
	return cpaapi.RequestInterceptResponse{Body: updated}, shape, true
}

func (e *AdaptiveWeeklyOverdraftExperiment) classifyRequestShape(raw json.RawMessage, authID string) (AdaptiveRequestShape, bool) {
	var last struct {
		Type string `json:"type"`
		Role string `json:"role"`
	}
	if errLast := json.Unmarshal(raw, &last); errLast != nil {
		return "", false
	}
	if last.Type == "message" && last.Role == "user" {
		return AdaptiveRequestShapeUserMessage, true
	}
	if last.Type != "function_call_output" && last.Type != "custom_tool_call_output" {
		return "", false
	}
	if e.toolEnabled == nil || !e.toolEnabled() || !adaptiveCanarySelected(authID, e.toolOutputPercent()) {
		return "", false
	}
	return AdaptiveRequestShapeToolOutput, true
}

func (e *AdaptiveWeeklyOverdraftExperiment) toolOutputPercent() int {
	if e == nil || e.toolPercent == nil {
		return defaultAdaptiveToolOutputPercent
	}
	return min(max(e.toolPercent(), 1), 100)
}

func adaptiveCanarySelected(value string, percent int) bool {
	value = strings.TrimSpace(value)
	if value == "" || percent <= 0 {
		return false
	}
	if percent >= 100 {
		return true
	}
	sum := sha256.Sum256([]byte(value))
	bucket := int(binary.BigEndian.Uint32(sum[:4])%100) + 1
	return bucket <= percent
}

func (e *AdaptiveWeeklyOverdraftExperiment) strategyForAuthID(authID string, now time.Time) (AdaptiveOverdraftStrategy, bool) {
	fingerprint, ok := adaptiveAuthFingerprintKey(authID)
	if !ok {
		return "", false
	}
	e.mu.RLock()
	record, exists := e.records[string(fingerprint[:])]
	e.mu.RUnlock()
	if !exists || record.Phase == AdaptivePhaseIdle || record.Phase == AdaptivePhaseExhausted || record.Phase == AdaptivePhaseHardStopped || !validAdaptiveStrategy(record.Strategy) || record.Strategy == "" {
		return "", false
	}
	if !record.ResetAt.IsZero() && !now.Before(record.ResetAt) {
		return "", false
	}
	if record.QuotaObservedAt.IsZero() || record.QuotaObservedAt.After(now.Add(time.Minute)) || now.Sub(record.QuotaObservedAt) > adaptiveOverdraftQuotaFreshness {
		return "", false
	}
	return record.Strategy, true
}

func (e *AdaptiveWeeklyOverdraftExperiment) adaptiveToolPairs(strategy AdaptiveOverdraftStrategy) ([]byte, bool) {
	pairCount := adaptiveStrategyPairCount(strategy)
	if pairCount == 0 {
		return nil, false
	}
	items := make([]json.RawMessage, 0, pairCount*2)
	for index := 0; index < pairCount; index++ {
		callID, ok := e.nextCallID(strategy)
		if !ok {
			return nil, false
		}
		call, errCall := json.Marshal(map[string]any{
			"type": "custom_tool_call", "name": "exec", "call_id": callID, "input": experimentalExecInput,
		})
		output, errOutput := json.Marshal(map[string]any{
			"type": "custom_tool_call_output", "call_id": callID,
			"output": []map[string]string{{"type": "input_text", "text": "Script completed\nWall time 0.0 seconds\nOutput:\n"}},
		})
		if errCall != nil || errOutput != nil {
			return nil, false
		}
		items = append(items, call, output)
	}
	joined, errJoin := json.Marshal(items)
	if errJoin != nil || len(joined) < 2 {
		return nil, false
	}
	return joined[1 : len(joined)-1], true
}

func (e *AdaptiveWeeklyOverdraftExperiment) nextCallID(strategy AdaptiveOverdraftStrategy) (string, bool) {
	if e != nil && e.newCallID != nil {
		return e.newCallID(strategy)
	}
	return newAdaptiveExperimentalCallID(strategy)
}

func (e *AdaptiveWeeklyOverdraftExperiment) bodyLimit() int {
	if e != nil && e.maxBodyBytes > 0 {
		return e.maxBodyBytes
	}
	return defaultExperimentalRequestBodyLimit
}

func adaptiveStrategyPairCount(strategy AdaptiveOverdraftStrategy) int {
	switch strategy {
	case AdaptiveStrategyS1:
		return 1
	case AdaptiveStrategyS2:
		return 2
	case AdaptiveStrategyS4:
		return 4
	default:
		return 0
	}
}

func newAdaptiveExperimentalCallID(strategy AdaptiveOverdraftStrategy) (string, bool) {
	if adaptiveStrategyPairCount(strategy) == 0 {
		return "", false
	}
	var random [12]byte
	if _, errRead := rand.Read(random[:]); errRead != nil {
		return "", false
	}
	return "call_cpa_adaptive_" + string(strategy) + "_" + hex.EncodeToString(random[:]), true
}
