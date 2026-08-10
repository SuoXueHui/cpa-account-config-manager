package manager

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"

	"cpa-account-config-manager/internal/cpaapi"
)

type ExperimentalSettings struct {
	WeeklyOverdraftEnabled         bool `json:"weekly_overdraft_enabled" yaml:"weekly_overdraft_enabled"`
	AdaptiveWeeklyOverdraftEnabled bool `json:"adaptive_weekly_overdraft_enabled" yaml:"adaptive_weekly_overdraft_enabled"`
	AdaptiveTokenDrainEnabled      bool `json:"adaptive_token_drain_enabled" yaml:"adaptive_token_drain_enabled"`
	AdaptiveTokenDrainPercent      int  `json:"adaptive_token_drain_percent" yaml:"adaptive_token_drain_percent"`
	AdaptiveTokenDrainMaxSessions  int  `json:"adaptive_token_drain_max_sessions" yaml:"adaptive_token_drain_max_sessions"`
	AdaptiveToolOutputEnabled      bool `json:"adaptive_tool_output_enabled" yaml:"adaptive_tool_output_enabled"`
	AdaptiveToolOutputPercent      int  `json:"adaptive_tool_output_percent" yaml:"adaptive_tool_output_percent"`
	AgentIdentityEnabled           bool `json:"agent_identity_enabled" yaml:"agent_identity_enabled"`
	AutoModelWhitelistEnabled      bool `json:"auto_model_whitelist_enabled" yaml:"auto_model_whitelist_enabled"`
}

const (
	defaultAdaptiveTokenDrainPercent     = 20
	defaultAdaptiveTokenDrainMaxSessions = 8
	defaultAdaptiveToolOutputPercent     = 10
)

var (
	ErrOverdraftModesMutuallyExclusive    = errors.New("weekly overdraft modes are mutually exclusive")
	ErrAdaptiveWeeklyOverdraftUnavailable = errors.New("adaptive weekly overdraft requires request lifecycle schema v2")
)

func (s *ExperimentalSettingsService) AgentIdentityEnabled() bool {
	if s == nil {
		return false
	}
	s.mu.RLock()
	enabled := s.settings.AgentIdentityEnabled
	s.mu.RUnlock()
	return enabled
}

func (s *ExperimentalSettingsService) AutoModelWhitelistEnabled() bool {
	return true
}

func normalizeExperimentalSettings(settings ExperimentalSettings) ExperimentalSettings {
	settings.AutoModelWhitelistEnabled = true
	settings.AdaptiveTokenDrainPercent = normalizeExperimentalPercent(settings.AdaptiveTokenDrainPercent, defaultAdaptiveTokenDrainPercent)
	settings.AdaptiveTokenDrainMaxSessions = normalizeExperimentalBoundedInt(settings.AdaptiveTokenDrainMaxSessions, defaultAdaptiveTokenDrainMaxSessions, 1, 64)
	settings.AdaptiveToolOutputPercent = normalizeExperimentalPercent(settings.AdaptiveToolOutputPercent, defaultAdaptiveToolOutputPercent)
	return settings
}

func normalizeExperimentalPercent(value, fallback int) int {
	return normalizeExperimentalBoundedInt(value, fallback, 1, 100)
}

func normalizeExperimentalBoundedInt(value, fallback, minimum, maximum int) int {
	if value == 0 {
		return fallback
	}
	return min(max(value, minimum), maximum)
}

type ExperimentalSettingsSnapshot struct {
	Settings                                 ExperimentalSettings `json:"settings"`
	AdaptiveWeeklyOverdraftAvailable         bool                 `json:"adaptive_weekly_overdraft_available"`
	AdaptiveWeeklyOverdraftUnavailableReason string               `json:"adaptive_weekly_overdraft_unavailable_reason,omitempty"`
	ConfigurationWarning                     string               `json:"configuration_warning,omitempty"`
	StorageError                             string               `json:"storage_error,omitempty"`
}

type ExperimentalSettingsService struct {
	mu                     sync.RWMutex
	storeMu                sync.Mutex
	store                  string
	settings               ExperimentalSettings
	storageErr             string
	configurationWarning   string
	configured             bool
	hostSchema             uint32
	weeklyOverdraftEnabled atomic.Bool
	adaptiveEnabled        atomic.Bool
	adaptiveDrainEnabled   atomic.Bool
	adaptiveDrainPercent   atomic.Int64
	adaptiveDrainSessions  atomic.Int64
	adaptiveToolEnabled    atomic.Bool
	adaptiveToolPercent    atomic.Int64
}

func NewExperimentalSettingsService() *ExperimentalSettingsService {
	config := normalizeConfig(Config{})
	return &ExperimentalSettingsService{store: experimentalSettingsStorePath(config.DataDir)}
}

func (s *ExperimentalSettingsService) Configure(config Config) {
	s.ConfigureHost(config, cpaapi.SchemaVersion)
}

func (s *ExperimentalSettingsService) ConfigureHost(config Config, hostSchema uint32) {
	if s == nil {
		return
	}
	config = normalizeConfig(config)
	hostSchema = normalizeHostSchemaVersion(hostSchema)
	storePath := experimentalSettingsStorePath(config.DataDir)
	s.mu.RLock()
	sameStore := s.configured && s.store == storePath && s.hostSchema == hostSchema
	s.mu.RUnlock()
	if sameStore {
		if config.ExperimentalSettings != nil {
			s.applyConfiguredSettings(storePath, *config.ExperimentalSettings, hostSchema)
		}
		return
	}
	settings := normalizeExperimentalSettings(ExperimentalSettings{})
	storageErr := ""
	configurationWarning := ""
	loaded, errLoad := loadExperimentalSettings(storePath)
	if errLoad == nil {
		settings = normalizeExperimentalSettings(loaded)
		settings, configurationWarning = normalizeConfiguredOverdraftModes(settings)
		if hostSchema < cpaapi.SchemaVersion && settings.AdaptiveWeeklyOverdraftEnabled {
			settings.AdaptiveWeeklyOverdraftEnabled = false
			configurationWarning = "adaptive_weekly_overdraft_requires_host_schema_v2"
		}
	} else if !errors.Is(errLoad, os.ErrNotExist) {
		storageErr = "experimental settings could not be loaded"
	}
	if config.ExperimentalSettings != nil {
		settings = normalizeExperimentalSettings(*config.ExperimentalSettings)
		settings, configurationWarning = normalizeConfiguredOverdraftModes(settings)
		if hostSchema < cpaapi.SchemaVersion && settings.AdaptiveWeeklyOverdraftEnabled {
			settings.AdaptiveWeeklyOverdraftEnabled = false
			configurationWarning = "adaptive_weekly_overdraft_requires_host_schema_v2"
		}
		s.storeMu.Lock()
		if errSave := saveExperimentalSettings(storePath, settings); errSave != nil {
			storageErr = "experimental settings could not be persisted"
		}
		s.storeMu.Unlock()
	}
	s.mu.Lock()
	s.store = storePath
	s.settings = settings
	s.storageErr = storageErr
	s.configurationWarning = configurationWarning
	s.configured = true
	s.hostSchema = hostSchema
	s.mu.Unlock()
	s.weeklyOverdraftEnabled.Store(settings.WeeklyOverdraftEnabled)
	s.adaptiveEnabled.Store(settings.AdaptiveWeeklyOverdraftEnabled)
	s.storeTokenFirstSettings(settings)
}

func (s *ExperimentalSettingsService) Snapshot() ExperimentalSettingsSnapshot {
	if s == nil {
		return ExperimentalSettingsSnapshot{}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	available := s.hostSchema >= cpaapi.SchemaVersion
	reason := ""
	if !available {
		reason = "host_schema_v2_required"
	}
	return ExperimentalSettingsSnapshot{
		Settings:                         normalizeExperimentalSettings(s.settings),
		AdaptiveWeeklyOverdraftAvailable: available, AdaptiveWeeklyOverdraftUnavailableReason: reason,
		ConfigurationWarning: s.configurationWarning, StorageError: s.storageErr,
	}
}

func (s *ExperimentalSettingsService) WeeklyOverdraftEnabled() bool {
	if s == nil {
		return false
	}
	return s.weeklyOverdraftEnabled.Load()
}

func (s *ExperimentalSettingsService) AdaptiveWeeklyOverdraftEnabled() bool {
	if s == nil {
		return false
	}
	return s.adaptiveEnabled.Load()
}

func (s *ExperimentalSettingsService) AdaptiveTokenDrainEnabled() bool {
	return s != nil && s.adaptiveEnabled.Load() && s.adaptiveDrainEnabled.Load()
}

func (s *ExperimentalSettingsService) AdaptiveTokenDrainPercent() int {
	if s == nil {
		return defaultAdaptiveTokenDrainPercent
	}
	return int(s.adaptiveDrainPercent.Load())
}

func (s *ExperimentalSettingsService) AdaptiveTokenDrainMaxSessions() int {
	if s == nil {
		return defaultAdaptiveTokenDrainMaxSessions
	}
	return int(s.adaptiveDrainSessions.Load())
}

func (s *ExperimentalSettingsService) AdaptiveToolOutputEnabled() bool {
	return s != nil && s.adaptiveEnabled.Load() && s.adaptiveToolEnabled.Load()
}

func (s *ExperimentalSettingsService) AdaptiveToolOutputPercent() int {
	if s == nil {
		return defaultAdaptiveToolOutputPercent
	}
	return int(s.adaptiveToolPercent.Load())
}

func (s *ExperimentalSettingsService) Set(settings ExperimentalSettings) (ExperimentalSettingsSnapshot, error) {
	if s == nil {
		return ExperimentalSettingsSnapshot{}, fmt.Errorf("experimental settings are unavailable")
	}
	s.mu.RLock()
	storePath := s.store
	configured := s.configured
	s.mu.RUnlock()
	if !configured || strings.TrimSpace(storePath) == "" {
		return ExperimentalSettingsSnapshot{}, fmt.Errorf("experimental settings storage is unavailable")
	}
	settings = normalizeExperimentalSettings(settings)
	if settings.WeeklyOverdraftEnabled && settings.AdaptiveWeeklyOverdraftEnabled {
		return ExperimentalSettingsSnapshot{}, ErrOverdraftModesMutuallyExclusive
	}
	s.mu.RLock()
	hostSchema := s.hostSchema
	s.mu.RUnlock()
	if settings.AdaptiveWeeklyOverdraftEnabled && hostSchema < cpaapi.SchemaVersion {
		return ExperimentalSettingsSnapshot{}, ErrAdaptiveWeeklyOverdraftUnavailable
	}
	s.storeMu.Lock()
	errSave := saveExperimentalSettings(storePath, settings)
	s.storeMu.Unlock()
	if errSave != nil {
		return ExperimentalSettingsSnapshot{}, fmt.Errorf("save experimental settings: %w", errSave)
	}
	s.mu.Lock()
	s.settings = settings
	s.storageErr = ""
	s.configurationWarning = ""
	s.mu.Unlock()
	s.weeklyOverdraftEnabled.Store(settings.WeeklyOverdraftEnabled)
	s.adaptiveEnabled.Store(settings.AdaptiveWeeklyOverdraftEnabled)
	s.storeTokenFirstSettings(settings)
	return s.Snapshot(), nil
}

func normalizeConfiguredOverdraftModes(settings ExperimentalSettings) (ExperimentalSettings, string) {
	if !settings.WeeklyOverdraftEnabled || !settings.AdaptiveWeeklyOverdraftEnabled {
		return settings, ""
	}
	settings.AdaptiveWeeklyOverdraftEnabled = false
	return settings, "overdraft_modes_conflicted_original_preserved"
}

func (s *ExperimentalSettingsService) applyConfiguredSettings(storePath string, settings ExperimentalSettings, hostSchema uint32) {
	settings = normalizeExperimentalSettings(settings)
	settings, warning := normalizeConfiguredOverdraftModes(settings)
	if hostSchema < cpaapi.SchemaVersion && settings.AdaptiveWeeklyOverdraftEnabled {
		settings.AdaptiveWeeklyOverdraftEnabled = false
		warning = "adaptive_weekly_overdraft_requires_host_schema_v2"
	}
	s.storeMu.Lock()
	errSave := saveExperimentalSettings(storePath, settings)
	s.storeMu.Unlock()
	s.mu.Lock()
	if errSave != nil {
		s.storageErr = "experimental settings could not be persisted"
	} else {
		s.settings = settings
		s.storageErr = ""
		s.configurationWarning = warning
	}
	s.mu.Unlock()
	if errSave == nil {
		s.weeklyOverdraftEnabled.Store(settings.WeeklyOverdraftEnabled)
		s.adaptiveEnabled.Store(settings.AdaptiveWeeklyOverdraftEnabled)
		s.storeTokenFirstSettings(settings)
	}
}

func (s *ExperimentalSettingsService) storeTokenFirstSettings(settings ExperimentalSettings) {
	if s == nil {
		return
	}
	settings = normalizeExperimentalSettings(settings)
	s.adaptiveDrainEnabled.Store(settings.AdaptiveTokenDrainEnabled)
	s.adaptiveDrainPercent.Store(int64(settings.AdaptiveTokenDrainPercent))
	s.adaptiveDrainSessions.Store(int64(settings.AdaptiveTokenDrainMaxSessions))
	s.adaptiveToolEnabled.Store(settings.AdaptiveToolOutputEnabled)
	s.adaptiveToolPercent.Store(int64(settings.AdaptiveToolOutputPercent))
}
