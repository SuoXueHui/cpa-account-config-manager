package manager

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestAdaptiveWeeklyOverdraftStoreRoundTripExcludesSecretsAndUsesPrivatePermissions(t *testing.T) {
	path := adaptiveWeeklyOverdraftStorePath(t.TempDir())
	fingerprint := adaptiveAuthFingerprint("secret-auth-id")
	records := map[string]adaptiveOverdraftRecord{
		fingerprint: {
			Fingerprint: fingerprint, Phase: AdaptivePhaseActiveS2, Strategy: AdaptiveStrategyS2,
			PostThresholdSuccesses: 5, PostThresholdTokens: 42,
			LastObservedAt: time.Date(2026, 7, 29, 0, 0, 0, 0, time.UTC),
		},
	}
	if errSave := saveAdaptiveWeeklyOverdraftState(path, records, records[fingerprint].LastObservedAt); errSave != nil {
		t.Fatalf("saveAdaptiveWeeklyOverdraftState() error = %v", errSave)
	}
	raw, errRead := os.ReadFile(path)
	if errRead != nil {
		t.Fatalf("ReadFile() error = %v", errRead)
	}
	if bytes.Contains(raw, []byte("secret-auth-id")) {
		t.Fatal("persisted state contains raw AuthID")
	}
	info, errStat := os.Stat(path)
	if errStat != nil {
		t.Fatalf("Stat() error = %v", errStat)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %o", info.Mode().Perm())
	}
	loaded, errLoad := loadAdaptiveWeeklyOverdraftState(path, records[fingerprint].LastObservedAt)
	if errLoad != nil {
		t.Fatalf("loadAdaptiveWeeklyOverdraftState() error = %v", errLoad)
	}
	if got := loaded[fingerprint]; got.Phase != AdaptivePhaseActiveS2 || got.PostThresholdSuccesses != 5 || got.PostThresholdTokens != 42 {
		t.Fatalf("loaded = %#v", got)
	}
}

func TestAdaptiveWeeklyOverdraftStorePrunesExpiredAndBoundsRecords(t *testing.T) {
	now := time.Date(2026, 7, 29, 0, 0, 0, 0, time.UTC)
	records := make(map[string]adaptiveOverdraftRecord, maxAdaptiveOverdraftAccounts+2)
	for index := 0; index < maxAdaptiveOverdraftAccounts+1; index++ {
		fingerprint := adaptiveAuthFingerprint(strconv.Itoa(index + 1))
		records[fingerprint] = adaptiveOverdraftRecord{Fingerprint: fingerprint, Phase: AdaptivePhaseArmed, Strategy: AdaptiveStrategyS1, LastObservedAt: now.Add(-time.Duration(index) * time.Second)}
	}
	expired := adaptiveAuthFingerprint("expired")
	records[expired] = adaptiveOverdraftRecord{Fingerprint: expired, Phase: AdaptivePhaseArmed, LastObservedAt: now.Add(-32 * 24 * time.Hour)}
	path := adaptiveWeeklyOverdraftStorePath(t.TempDir())
	if errSave := saveAdaptiveWeeklyOverdraftState(path, records, now); errSave != nil {
		t.Fatalf("saveAdaptiveWeeklyOverdraftState() error = %v", errSave)
	}
	loaded, errLoad := loadAdaptiveWeeklyOverdraftState(path, now)
	if errLoad != nil {
		t.Fatalf("loadAdaptiveWeeklyOverdraftState() error = %v", errLoad)
	}
	if len(loaded) != maxAdaptiveOverdraftAccounts {
		t.Fatalf("records = %d", len(loaded))
	}
	if _, exists := loaded[expired]; exists {
		t.Fatal("expired record was retained")
	}
}

func TestAdaptiveWeeklyOverdraftStoreRejectsCorruptAndFutureVersions(t *testing.T) {
	for name, raw := range map[string][]byte{
		"corrupt": []byte(`{"version":1,"records":`),
		"future":  []byte(`{"version":2,"records":[]}`),
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "adaptive-weekly-overdraft.json")
			if errWrite := os.WriteFile(path, raw, 0o600); errWrite != nil {
				t.Fatalf("WriteFile() error = %v", errWrite)
			}
			if _, errLoad := loadAdaptiveWeeklyOverdraftState(path, time.Now()); errLoad == nil {
				t.Fatal("load unexpectedly succeeded")
			}
		})
	}
}

func TestAdaptiveWeeklyOverdraftStoreSanitizesInvalidRecords(t *testing.T) {
	path := filepath.Join(t.TempDir(), "adaptive-weekly-overdraft.json")
	raw, _ := json.Marshal(map[string]any{
		"version": 1,
		"records": []map[string]any{
			{"fingerprint": strings.Repeat("z", 64), "phase": "armed", "strategy": "s1"},
			{"fingerprint": adaptiveAuthFingerprint("valid"), "phase": "unknown", "strategy": "s9"},
		},
	})
	if errWrite := os.WriteFile(path, raw, 0o600); errWrite != nil {
		t.Fatalf("WriteFile() error = %v", errWrite)
	}
	loaded, errLoad := loadAdaptiveWeeklyOverdraftState(path, time.Now())
	if errLoad != nil {
		t.Fatalf("loadAdaptiveWeeklyOverdraftState() error = %v", errLoad)
	}
	if len(loaded) != 0 {
		t.Fatalf("invalid records loaded = %#v", loaded)
	}
}
