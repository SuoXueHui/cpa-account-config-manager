package manager

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"cpa-account-config-manager/internal/cpaapi"
)

func TestExperimentalSettingsRejectMutuallyExclusiveOverdraftModes(t *testing.T) {
	service := NewExperimentalSettingsService()
	service.ConfigureHost(Config{DataDir: t.TempDir()}, cpaapi.SchemaVersion)
	_, errSet := service.Set(ExperimentalSettings{
		WeeklyOverdraftEnabled:         true,
		AdaptiveWeeklyOverdraftEnabled: true,
	})
	if !errors.Is(errSet, ErrOverdraftModesMutuallyExclusive) {
		t.Fatalf("Set() error = %v", errSet)
	}
}

func TestExperimentalSettingsTokenFirstDefaultsAndNormalization(t *testing.T) {
	service := NewExperimentalSettingsService()
	service.ConfigureHost(Config{DataDir: t.TempDir()}, cpaapi.SchemaVersion)

	snapshot := service.Snapshot()
	if snapshot.Settings.AdaptiveTokenDrainEnabled || snapshot.Settings.AdaptiveToolOutputEnabled {
		t.Fatalf("token-first features must default off: %#v", snapshot.Settings)
	}
	if snapshot.Settings.AdaptiveTokenDrainPercent != 20 || snapshot.Settings.AdaptiveTokenDrainMaxSessions != 8 || snapshot.Settings.AdaptiveToolOutputPercent != 10 {
		t.Fatalf("token-first defaults = %#v", snapshot.Settings)
	}

	updated, errSet := service.Set(ExperimentalSettings{
		AdaptiveWeeklyOverdraftEnabled: true,
		AdaptiveTokenDrainEnabled:      true,
		AdaptiveTokenDrainPercent:      -3,
		AdaptiveTokenDrainMaxSessions:  99,
		AdaptiveToolOutputEnabled:      true,
		AdaptiveToolOutputPercent:      101,
	})
	if errSet != nil {
		t.Fatalf("Set() error = %v", errSet)
	}
	if updated.Settings.AdaptiveTokenDrainPercent != 1 || updated.Settings.AdaptiveTokenDrainMaxSessions != 64 || updated.Settings.AdaptiveToolOutputPercent != 100 {
		t.Fatalf("normalized settings = %#v", updated.Settings)
	}
	if !service.AdaptiveTokenDrainEnabled() || service.AdaptiveTokenDrainPercent() != 1 || service.AdaptiveTokenDrainMaxSessions() != 64 ||
		!service.AdaptiveToolOutputEnabled() || service.AdaptiveToolOutputPercent() != 100 {
		t.Fatal("atomic token-first settings do not match the saved snapshot")
	}
}

func TestExperimentalSettingsTokenFirstSettingsPersistAcrossRestart(t *testing.T) {
	dataDir := t.TempDir()
	first := NewExperimentalSettingsService()
	first.ConfigureHost(Config{DataDir: dataDir}, cpaapi.SchemaVersion)
	_, errSet := first.Set(ExperimentalSettings{
		AdaptiveWeeklyOverdraftEnabled: true,
		AdaptiveTokenDrainEnabled:      true,
		AdaptiveTokenDrainPercent:      35,
		AdaptiveTokenDrainMaxSessions:  12,
		AdaptiveToolOutputEnabled:      true,
		AdaptiveToolOutputPercent:      25,
	})
	if errSet != nil {
		t.Fatalf("Set() error = %v", errSet)
	}

	restarted := NewExperimentalSettingsService()
	restarted.ConfigureHost(Config{DataDir: dataDir}, cpaapi.SchemaVersion)
	snapshot := restarted.Snapshot().Settings
	if !snapshot.AdaptiveTokenDrainEnabled || snapshot.AdaptiveTokenDrainPercent != 35 || snapshot.AdaptiveTokenDrainMaxSessions != 12 ||
		!snapshot.AdaptiveToolOutputEnabled || snapshot.AdaptiveToolOutputPercent != 25 {
		t.Fatalf("restarted settings = %#v", snapshot)
	}
}

func TestExperimentalSettingsRejectAdaptiveModeOnLegacyHost(t *testing.T) {
	service := NewExperimentalSettingsService()
	service.ConfigureHost(Config{DataDir: t.TempDir()}, cpaapi.LegacySchemaVersion)
	_, errSet := service.Set(ExperimentalSettings{AdaptiveWeeklyOverdraftEnabled: true})
	if !errors.Is(errSet, ErrAdaptiveWeeklyOverdraftUnavailable) {
		t.Fatalf("Set() error = %v", errSet)
	}
}

func TestExperimentalSettingsConflictingStartupConfigKeepsOriginalMode(t *testing.T) {
	service := NewExperimentalSettingsService()
	settings := ExperimentalSettings{WeeklyOverdraftEnabled: true, AdaptiveWeeklyOverdraftEnabled: true}
	service.ConfigureHost(Config{DataDir: t.TempDir(), ExperimentalSettings: &settings}, cpaapi.SchemaVersion)
	snapshot := service.Snapshot()
	if !snapshot.Settings.WeeklyOverdraftEnabled || snapshot.Settings.AdaptiveWeeklyOverdraftEnabled {
		t.Fatalf("settings = %#v", snapshot.Settings)
	}
	if snapshot.ConfigurationWarning != "overdraft_modes_conflicted_original_preserved" {
		t.Fatalf("configuration_warning = %q", snapshot.ConfigurationWarning)
	}
}

func TestExperimentalSettingsSnapshotReportsAdaptiveAvailability(t *testing.T) {
	service := NewExperimentalSettingsService()
	service.ConfigureHost(Config{DataDir: t.TempDir()}, cpaapi.SchemaVersion)
	if snapshot := service.Snapshot(); !snapshot.AdaptiveWeeklyOverdraftAvailable || snapshot.AdaptiveWeeklyOverdraftUnavailableReason != "" {
		t.Fatalf("schema v2 snapshot = %#v", snapshot)
	}

	legacy := NewExperimentalSettingsService()
	legacy.ConfigureHost(Config{DataDir: t.TempDir()}, cpaapi.LegacySchemaVersion)
	if snapshot := legacy.Snapshot(); snapshot.AdaptiveWeeklyOverdraftAvailable || snapshot.AdaptiveWeeklyOverdraftUnavailableReason != "host_schema_v2_required" {
		t.Fatalf("legacy snapshot = %#v", snapshot)
	}
}

func TestExperimentalSettingsManagementRejectsInvalidAdaptiveWrites(t *testing.T) {
	path := "/v0/management/plugins/cpa-account-config-manager/experiments"
	for _, test := range []struct {
		name       string
		hostSchema uint32
		body       string
	}{
		{name: "mutually-exclusive", hostSchema: cpaapi.SchemaVersion, body: `{"weekly_overdraft_enabled":true,"adaptive_weekly_overdraft_enabled":true}`},
		{name: "legacy-host", hostSchema: cpaapi.LegacySchemaVersion, body: `{"adaptive_weekly_overdraft_enabled":true}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			app := NewApp(&fakeAuthHost{}, []byte("index"))
			defer app.Close()
			app.ConfigureHost([]byte("data_dir: "+t.TempDir()+"\n"), test.hostSchema)
			response := app.HandleManagement(context.Background(), cpaapi.ManagementRequest{
				Method: http.MethodPut, Path: path, Body: []byte(test.body),
			})
			if response.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d body=%s", response.StatusCode, response.Body)
			}
			if snapshot := app.experiments.Snapshot(); snapshot.Settings.WeeklyOverdraftEnabled || snapshot.Settings.AdaptiveWeeklyOverdraftEnabled {
				t.Fatalf("invalid write changed settings = %#v", snapshot.Settings)
			}
		})
	}
}

func TestExperimentalSettingsLegacyHostDisablesPersistedAdaptiveMode(t *testing.T) {
	dataDir := t.TempDir()
	current := NewExperimentalSettingsService()
	current.ConfigureHost(Config{DataDir: dataDir}, cpaapi.SchemaVersion)
	if _, errSet := current.Set(ExperimentalSettings{AdaptiveWeeklyOverdraftEnabled: true}); errSet != nil {
		t.Fatalf("Set() error = %v", errSet)
	}

	legacy := NewExperimentalSettingsService()
	legacy.ConfigureHost(Config{DataDir: dataDir}, cpaapi.LegacySchemaVersion)
	snapshot := legacy.Snapshot()
	if snapshot.Settings.AdaptiveWeeklyOverdraftEnabled {
		t.Fatal("legacy host loaded persisted adaptive mode")
	}
	if snapshot.ConfigurationWarning != "adaptive_weekly_overdraft_requires_host_schema_v2" {
		t.Fatalf("configuration_warning = %q", snapshot.ConfigurationWarning)
	}
}

func TestExperimentalSettingsDefaultDisabledAndPersistAcrossRestart(t *testing.T) {
	dataDir := t.TempDir()
	first := NewExperimentalSettingsService()
	first.Configure(Config{DataDir: dataDir})
	if snapshot := first.Snapshot(); snapshot.Settings.WeeklyOverdraftEnabled || snapshot.Settings.AgentIdentityEnabled ||
		!snapshot.Settings.AutoModelWhitelistEnabled || snapshot.Settings.Sub2APICreditUsageEnabled || snapshot.StorageError != "" {
		t.Fatalf("default snapshot = %#v", snapshot)
	}
	if _, errSet := first.Set(ExperimentalSettings{WeeklyOverdraftEnabled: true, AgentIdentityEnabled: true, AutoModelWhitelistEnabled: true, Sub2APICreditUsageEnabled: true}); errSet != nil {
		t.Fatalf("Set() error = %v", errSet)
	}

	restarted := NewExperimentalSettingsService()
	restarted.Configure(Config{DataDir: dataDir})
	if snapshot := restarted.Snapshot(); !snapshot.Settings.WeeklyOverdraftEnabled || !snapshot.Settings.AgentIdentityEnabled ||
		!snapshot.Settings.AutoModelWhitelistEnabled || !snapshot.Settings.Sub2APICreditUsageEnabled || snapshot.StorageError != "" {
		t.Fatalf("restarted snapshot = %#v", snapshot)
	}
}

func TestExperimentalSettingsConfigOverridePersists(t *testing.T) {
	dataDir := t.TempDir()
	settings := ExperimentalSettings{WeeklyOverdraftEnabled: true, AgentIdentityEnabled: true, AutoModelWhitelistEnabled: true, Sub2APICreditUsageEnabled: true}
	service := NewExperimentalSettingsService()
	service.Configure(Config{DataDir: dataDir, ExperimentalSettings: &settings})
	if !service.WeeklyOverdraftEnabled() {
		t.Fatal("config override did not enable weekly overdraft")
	}
	if !service.AgentIdentityEnabled() {
		t.Fatal("config override did not enable Agent Identity")
	}
	if !service.AutoModelWhitelistEnabled() {
		t.Fatal("config override did not enable automatic model whitelist")
	}
	if !service.Sub2APICreditUsageEnabled() {
		t.Fatal("config override did not enable Sub2API credit usage")
	}

	reloaded, errLoad := loadExperimentalSettings(experimentalSettingsStorePath(dataDir))
	if errLoad != nil {
		t.Fatalf("loadExperimentalSettings() error = %v", errLoad)
	}
	if !reloaded.WeeklyOverdraftEnabled || !reloaded.AgentIdentityEnabled || !reloaded.AutoModelWhitelistEnabled || !reloaded.Sub2APICreditUsageEnabled {
		t.Fatalf("persisted settings = %#v", reloaded)
	}
}

func TestExperimentalSettingsCorruptStateFailsClosed(t *testing.T) {
	dataDir := t.TempDir()
	if errMkdir := os.MkdirAll(dataDir, 0o700); errMkdir != nil {
		t.Fatalf("MkdirAll() error = %v", errMkdir)
	}
	if errWrite := os.WriteFile(experimentalSettingsStorePath(dataDir), []byte(`{"version":1,"settings":`), 0o600); errWrite != nil {
		t.Fatalf("WriteFile() error = %v", errWrite)
	}

	service := NewExperimentalSettingsService()
	service.Configure(Config{DataDir: dataDir})
	snapshot := service.Snapshot()
	if snapshot.Settings.WeeklyOverdraftEnabled || snapshot.Settings.AgentIdentityEnabled || !snapshot.Settings.AutoModelWhitelistEnabled || snapshot.Settings.Sub2APICreditUsageEnabled {
		t.Fatal("corrupt state changed built-in model discovery or enabled an experiment")
	}
	if snapshot.StorageError != "experimental settings could not be loaded" {
		t.Fatalf("storage_error = %q", snapshot.StorageError)
	}
}

func TestExperimentalSettingsManagementRoutesPersistAndValidate(t *testing.T) {
	dataDir := t.TempDir()
	app := NewApp(&fakeAuthHost{}, []byte("index"))
	defer app.Close()
	app.Configure([]byte("data_dir: " + dataDir + "\n"))
	path := "/v0/management/plugins/cpa-account-config-manager/experiments"

	response := app.HandleManagement(context.Background(), cpaapi.ManagementRequest{Method: http.MethodGet, Path: path})
	if response.StatusCode != http.StatusOK {
		t.Fatalf("GET status = %d body=%s", response.StatusCode, response.Body)
	}
	var initial ExperimentalSettingsSnapshot
	if errDecode := json.Unmarshal(response.Body, &initial); errDecode != nil {
		t.Fatalf("decode GET response: %v", errDecode)
	}
	if initial.Settings.WeeklyOverdraftEnabled || initial.Settings.AgentIdentityEnabled || !initial.Settings.AutoModelWhitelistEnabled || initial.Settings.Sub2APICreditUsageEnabled {
		t.Fatal("GET returned invalid default settings")
	}

	response = app.HandleManagement(context.Background(), cpaapi.ManagementRequest{
		Method: http.MethodPut,
		Path:   path,
		Body:   []byte(`{"weekly_overdraft_enabled":true,"agent_identity_enabled":true,"auto_model_whitelist_enabled":true,"sub2api_credit_usage_enabled":true}`),
	})
	if response.StatusCode != http.StatusOK {
		t.Fatalf("PUT status = %d body=%s", response.StatusCode, response.Body)
	}
	if !app.experiments.WeeklyOverdraftEnabled() {
		t.Fatal("PUT did not enable the experiment")
	}
	if !app.experiments.AgentIdentityEnabled() {
		t.Fatal("PUT did not enable Agent Identity")
	}
	if !app.experiments.AutoModelWhitelistEnabled() {
		t.Fatal("PUT did not enable automatic model whitelist")
	}
	if !app.experiments.Sub2APICreditUsageEnabled() || !app.creditUsage.Enabled() {
		t.Fatal("PUT did not enable Sub2API credit usage")
	}

	for name, body := range map[string][]byte{
		"malformed": []byte(`{"weekly_overdraft_enabled":`),
		"unknown":   []byte(`{"weekly_overdraft_enabled":true,"unexpected":true}`),
	} {
		t.Run(name, func(t *testing.T) {
			invalid := app.HandleManagement(context.Background(), cpaapi.ManagementRequest{Method: http.MethodPut, Path: path, Body: body})
			if invalid.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d body=%s", invalid.StatusCode, invalid.Body)
			}
		})
	}
}

func TestExperimentalSettingsEnableTransformationAndSurviveAppRestart(t *testing.T) {
	dataDir := t.TempDir()
	config := []byte("data_dir: " + dataDir + "\n")
	path := "/v0/management/plugins/cpa-account-config-manager/experiments"
	request := cpaapi.RequestInterceptRequest{
		ToFormat: "codex",
		Body:     []byte(`{"input":[{"type":"message","role":"user","content":"continue"}]}`),
	}

	first := NewApp(&fakeAuthHost{}, []byte("index"))
	first.Configure(config)
	if response := first.HandleRequestAfter(request); len(response.Body) != 0 {
		first.Close()
		t.Fatal("disabled experiment unexpectedly transformed the request")
	}
	response := first.HandleManagement(context.Background(), cpaapi.ManagementRequest{
		Method: http.MethodPut,
		Path:   path,
		Body:   []byte(`{"weekly_overdraft_enabled":true}`),
	})
	if response.StatusCode != http.StatusOK {
		first.Close()
		t.Fatalf("PUT status = %d body=%s", response.StatusCode, response.Body)
	}
	if !first.RequestInterceptionActive() || !first.RequestInterceptionAcceptsFormat("codex") {
		first.Close()
		t.Fatal("enabled experiment did not expose the live request-interceptor gate")
	}
	if transformed := first.HandleRequestAfter(request); !containsExperimentalToolPair(transformed.Body) {
		first.Close()
		t.Fatalf("enabled Hook body = %s", transformed.Body)
	}
	first.Close()

	restarted := NewApp(&fakeAuthHost{}, []byte("index"))
	defer restarted.Close()
	restarted.Configure(config)
	if !restarted.RequestInterceptionActive() || !restarted.RequestInterceptionAcceptsFormat("codex") {
		t.Fatal("persisted experiment did not re-arm the request interceptor")
	}
	if transformed := restarted.HandleRequestAfter(request); !containsExperimentalToolPair(transformed.Body) {
		t.Fatalf("restarted Hook body = %s", transformed.Body)
	}
}

func TestExperimentalSettingsManagementStorageFailureIsSafe(t *testing.T) {
	dataDir := t.TempDir()
	blockingPath := filepath.Join(dataDir, "not-a-directory")
	if errWrite := os.WriteFile(blockingPath, []byte("block"), 0o600); errWrite != nil {
		t.Fatalf("WriteFile() error = %v", errWrite)
	}
	app := NewApp(&fakeAuthHost{}, []byte("index"))
	defer app.Close()
	app.Configure([]byte("data_dir: " + blockingPath + "\n"))
	response := app.HandleManagement(context.Background(), cpaapi.ManagementRequest{
		Method: http.MethodPut,
		Path:   "/v0/management/plugins/cpa-account-config-manager/experiments",
		Body:   []byte(`{"weekly_overdraft_enabled":true}`),
	})
	if response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d body=%s", response.StatusCode, response.Body)
	}
	if string(response.Body) != `{"error":"experimental settings could not be persisted"}` {
		t.Fatalf("unsafe error body = %s", response.Body)
	}
}

func containsExperimentalToolPair(body []byte) bool {
	if len(body) == 0 {
		return false
	}
	var document struct {
		Input []struct {
			Type   string `json:"type"`
			CallID string `json:"call_id"`
		} `json:"input"`
	}
	if errDecode := json.Unmarshal(body, &document); errDecode != nil || len(document.Input) < 2 {
		return false
	}
	call := document.Input[len(document.Input)-2]
	output := document.Input[len(document.Input)-1]
	return call.Type == "custom_tool_call" && output.Type == "custom_tool_call_output" &&
		strings.HasPrefix(call.CallID, "call_cpa_overdraft_") && call.CallID == output.CallID
}
