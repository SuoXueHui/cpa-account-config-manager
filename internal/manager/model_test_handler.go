package manager

import (
	"context"
	"errors"
	"net/http"
	"time"

	"cpa-account-config-manager/internal/cpaapi"
)

func (a *App) handleAccountModelTest(ctx context.Context, req cpaapi.ManagementRequest) cpaapi.ManagementResponse {
	var request ModelTestRequest
	if errDecode := decodeJSONRequest(req.Body, &request); errDecode != nil {
		return jsonResponse(http.StatusBadRequest, map[string]any{"error": errDecode.Error()})
	}
	if request.ExperimentalWeeklyOverdraft && request.ExperimentalAdaptiveWeeklyOverdraft {
		return jsonResponse(http.StatusBadRequest, map[string]any{"error": ErrOverdraftModesMutuallyExclusive.Error()})
	}
	if request.ExperimentalWeeklyOverdraft && !a.experiments.WeeklyOverdraftEnabled() {
		return jsonResponse(http.StatusConflict, map[string]any{"error": "weekly overdraft experiment is not enabled"})
	}
	if request.ExperimentalAdaptiveWeeklyOverdraft && !a.experiments.AdaptiveWeeklyOverdraftEnabled() {
		return jsonResponse(http.StatusConflict, map[string]any{"error": "adaptive weekly overdraft experiment is not enabled"})
	}
	if request.ExperimentalAdaptiveWeeklyOverdraft && request.AdaptiveWeeklyOverdraftStrategy != "" && adaptiveStrategyPairCount(request.AdaptiveWeeklyOverdraftStrategy) == 0 {
		return jsonResponse(http.StatusBadRequest, map[string]any{"error": "adaptive weekly overdraft strategy is invalid"})
	}
	managementKey := resolveManagementKey(req.Headers)
	if managementKey == "" {
		return jsonResponse(http.StatusUnauthorized, map[string]any{"error": "management key is unavailable"})
	}
	config := a.configSnapshot()
	request.DetectRestrictedModels = a.experiments.AutoModelWhitelistEnabled()
	var result ModelTestResult
	var errTest error
	if request.ExperimentalAdaptiveWeeklyOverdraft && request.AdaptiveWeeklyOverdraftStrategy == "" {
		result, errTest = a.runAdaptiveManualModelTest(ctx, request, config.ManagementBaseURL, managementKey, req.HostCallbackID)
	} else {
		result, errTest = a.modelTests.Run(ctx, request, config.ManagementBaseURL, managementKey, req.HostCallbackID)
	}
	if errTest != nil {
		managementKey = ""
		switch {
		case errors.Is(errTest, ErrModelTestAccountNotFound):
			return jsonResponse(http.StatusNotFound, map[string]any{"error": ErrModelTestAccountNotFound.Error()})
		case errors.Is(errTest, ErrModelTestBusy):
			return jsonResponse(http.StatusTooManyRequests, map[string]any{"error": ErrModelTestBusy.Error()})
		case errors.Is(errTest, ErrManagementBaseURLInvalid):
			return jsonResponse(http.StatusServiceUnavailable, map[string]any{"error": ErrManagementBaseURLInvalid.Error()})
		default:
			return jsonResponse(http.StatusBadRequest, map[string]any{"error": errTest.Error()})
		}
	}
	if request.ExperimentalAdaptiveWeeklyOverdraft && request.AdaptiveWeeklyOverdraftStrategy != "" {
		a.adaptiveOverdraft.ObserveProbeResultForAccountID(request.AccountID, request.AdaptiveWeeklyOverdraftStrategy, result)
	}
	if request.DetectRestrictedModels && len(result.CompatibleModels) > 0 {
		result.ModelPolicy = a.applyDetectedModelWhitelist(ctx, result.AccountID, result.CompatibleModels, config, managementKey, OperationSourceManual)
	}
	managementKey = ""
	a.recordModelTest(result, OperationSourceManual)
	_ = a.inspection.RecordManualModelTest(ctx, result)
	return jsonResponse(http.StatusOK, result)
}

func (a *App) runAdaptiveManualModelTest(ctx context.Context, request ModelTestRequest, managementBaseURL, managementKey, hostCallbackID string) (ModelTestResult, error) {
	models := safeAutomaticDisableProbeModels([]string{request.Model, defaultCodexFallbackModel, codexCompatibilityMiniModel})
	strategies := []AdaptiveOverdraftStrategy{AdaptiveStrategyS1, AdaptiveStrategyS2, AdaptiveStrategyS4}
	var attempts []ModelTestAttempt
	var final ModelTestResult
	for _, strategy := range strategies {
		strategyQuotaFailed := false
		for _, model := range models {
			nextRequest := request
			nextRequest.Model = model
			nextRequest.AdaptiveWeeklyOverdraftStrategy = strategy
			next, errRun := a.modelTests.Run(ctx, nextRequest, managementBaseURL, managementKey, hostCallbackID)
			if errRun != nil {
				return ModelTestResult{}, errRun
			}
			a.adaptiveOverdraft.ObserveProbeResultForAccountID(request.AccountID, strategy, next)
			attempts = append(attempts, next.Attempts...)
			final = next
			final.Attempts = append([]ModelTestAttempt(nil), attempts...)
			if final.PrimaryModel == "" {
				final.PrimaryModel = request.Model
			}
			if model != request.Model {
				final.FallbackModel = model
			}
			switch {
			case next.Status == "available", adaptiveProbeHardStop(next):
				return final, nil
			case adaptiveProbeUnsupported(next):
				continue
			case adaptiveProbeDefinitiveQuota(next):
				strategyQuotaFailed = true
			default:
				return final, nil
			}
			break
		}
		if !strategyQuotaFailed {
			return final, nil
		}
	}
	return final, nil
}

func (a *App) recordModelTest(result ModelTestResult, requestedSource ...string) {
	source := OperationSourceManual
	if len(requestedSource) > 0 && normalizeOperationSource(requestedSource[0]) != "" {
		source = normalizeOperationSource(requestedSource[0])
	}
	status := OperationStatusWarning
	succeeded := 0
	failed := 0
	skipped := 0
	switch result.Status {
	case "available":
		status = OperationStatusSucceeded
		succeeded = 1
	case "unavailable":
		status = OperationStatusFailed
		failed = 1
	case "unsupported":
		status = OperationStatusSkipped
		skipped = 1
	}
	finishedAt := result.TestedAt.Add(time.Duration(result.LatencyMS) * time.Millisecond)
	a.operations.Record(OperationEntry{
		Category: OperationCategoryAccount, Action: OperationActionModelTest, Status: status,
		Source: source, Scope: OperationScopeSingle, TargetID: result.AccountID, TargetCount: 1,
		Succeeded: succeeded, Failed: failed, Skipped: skipped, StartedAt: result.TestedAt, FinishedAt: finishedAt,
		ReasonCode: result.ReasonCode, Model: result.Model,
	})
}

func (a *App) runNewAccountModelProbe(ctx context.Context, account Account, managementKey string, hostCallbackID string) (ModelTestResult, error) {
	result, errRun := a.modelTests.Run(ctx, ModelTestRequest{
		AccountID: account.ID, Model: defaultOpenAIProbeModel, DetectRestrictedModels: true, SelectPolicyFallback: true,
	}, a.configSnapshot().ManagementBaseURL, managementKey, hostCallbackID)
	if errRun != nil {
		return result, errRun
	}
	if a.experiments.AutoModelWhitelistEnabled() && len(result.CompatibleModels) > 0 {
		result.ModelPolicy = a.applyDetectedModelWhitelist(ctx, result.AccountID, result.CompatibleModels, a.configSnapshot(), managementKey, OperationSourceBackground)
	}
	a.recordModelTest(result, OperationSourceBackground)
	_ = a.inspection.RecordModelTest(ctx, result, InspectionProbeSourceScan)
	return result, nil
}
