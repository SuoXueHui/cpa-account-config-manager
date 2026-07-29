package manager

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	adaptiveWeeklyOverdraftStoreVersion = 1
	adaptiveWeeklyOverdraftStoreName    = "adaptive-weekly-overdraft.json"
	maxAdaptiveOverdraftAccounts        = 10_000
	adaptiveOverdraftRecordTTL          = 31 * 24 * time.Hour
)

type persistedAdaptiveWeeklyOverdraftState struct {
	Version int                       `json:"version"`
	Records []adaptiveOverdraftRecord `json:"records"`
}

func adaptiveWeeklyOverdraftStorePath(dataDir string) string {
	return filepath.Join(dataDir, adaptiveWeeklyOverdraftStoreName)
}

func loadAdaptiveWeeklyOverdraftState(path string, now time.Time) (map[string]adaptiveOverdraftRecord, error) {
	raw, errRead := os.ReadFile(path)
	if errRead != nil {
		return nil, errRead
	}
	var persisted persistedAdaptiveWeeklyOverdraftState
	if errDecode := json.Unmarshal(raw, &persisted); errDecode != nil {
		return nil, fmt.Errorf("decode adaptive weekly overdraft state: %w", errDecode)
	}
	if persisted.Version != adaptiveWeeklyOverdraftStoreVersion {
		return nil, fmt.Errorf("unsupported adaptive weekly overdraft state version %d", persisted.Version)
	}
	now = now.UTC()
	records := make(map[string]adaptiveOverdraftRecord, min(len(persisted.Records), maxAdaptiveOverdraftAccounts))
	for _, candidate := range persisted.Records {
		record, ok := sanitizeAdaptiveOverdraftRecord(candidate, now)
		if !ok {
			continue
		}
		if len(records) >= maxAdaptiveOverdraftAccounts {
			break
		}
		records[record.Fingerprint] = record
	}
	return records, nil
}

func saveAdaptiveWeeklyOverdraftState(path string, records map[string]adaptiveOverdraftRecord, now time.Time) error {
	now = now.UTC()
	bounded := make([]adaptiveOverdraftRecord, 0, min(len(records), maxAdaptiveOverdraftAccounts))
	for _, candidate := range records {
		record, ok := sanitizeAdaptiveOverdraftRecord(candidate, now)
		if ok {
			bounded = append(bounded, record)
		}
	}
	sort.Slice(bounded, func(left, right int) bool {
		leftActivity := adaptiveOverdraftRecordActivity(bounded[left])
		rightActivity := adaptiveOverdraftRecordActivity(bounded[right])
		if leftActivity.Equal(rightActivity) {
			return bounded[left].Fingerprint < bounded[right].Fingerprint
		}
		return leftActivity.After(rightActivity)
	})
	if len(bounded) > maxAdaptiveOverdraftAccounts {
		bounded = bounded[:maxAdaptiveOverdraftAccounts]
	}
	return savePrivateJSON(path, persistedAdaptiveWeeklyOverdraftState{Version: adaptiveWeeklyOverdraftStoreVersion, Records: bounded})
}

func sanitizeAdaptiveOverdraftRecord(record adaptiveOverdraftRecord, now time.Time) (adaptiveOverdraftRecord, bool) {
	record.Fingerprint = strings.ToLower(strings.TrimSpace(record.Fingerprint))
	decoded, errDecode := hex.DecodeString(record.Fingerprint)
	if errDecode != nil || len(decoded) != 32 || !validAdaptivePhase(record.Phase) || !validAdaptiveStrategy(record.Strategy) {
		return adaptiveOverdraftRecord{}, false
	}
	if record.Phase == AdaptivePhaseIdle {
		record.Strategy = ""
	}
	if record.Phase == AdaptivePhaseArmed && record.Strategy == "" {
		record.Strategy = AdaptiveStrategyS1
	}
	record.PostThresholdSuccesses = maxInt64(0, record.PostThresholdSuccesses)
	record.PostThresholdTokens = maxInt64(0, record.PostThresholdTokens)
	record.ConsecutiveQuotaFailures = min(max(record.ConsecutiveQuotaFailures, 0), 1_000_000)
	record.HardStopReason = sanitizeAdaptiveHardStopReason(record.HardStopReason)
	if record.Phase != AdaptivePhaseHardStopped {
		record.HardStopReason = ""
	}
	activity := adaptiveOverdraftRecordActivity(record)
	if !activity.IsZero() && !now.IsZero() && activity.Before(now.Add(-adaptiveOverdraftRecordTTL)) {
		return adaptiveOverdraftRecord{}, false
	}
	return record, true
}

func adaptiveOverdraftRecordActivity(record adaptiveOverdraftRecord) time.Time {
	activity := record.LastObservedAt.UTC()
	for _, candidate := range []time.Time{record.LastSuccessAt, record.LastFailureAt} {
		candidate = candidate.UTC()
		if candidate.After(activity) {
			activity = candidate
		}
	}
	return activity
}

func validAdaptivePhase(phase AdaptiveOverdraftPhase) bool {
	switch phase {
	case AdaptivePhaseIdle, AdaptivePhaseArmed, AdaptivePhaseActiveS1, AdaptivePhaseActiveS2, AdaptivePhaseActiveS4, AdaptivePhaseExhausted, AdaptivePhaseHardStopped:
		return true
	default:
		return false
	}
}

func validAdaptiveStrategy(strategy AdaptiveOverdraftStrategy) bool {
	switch strategy {
	case "", AdaptiveStrategyS1, AdaptiveStrategyS2, AdaptiveStrategyS4:
		return true
	default:
		return false
	}
}
