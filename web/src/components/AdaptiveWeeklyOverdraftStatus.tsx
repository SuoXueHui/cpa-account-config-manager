import type { AdaptiveWeeklyOverdraftSummary } from "../types";
import { localeFormats, useI18n, type Locale } from "../i18n";

interface AdaptiveWeeklyOverdraftStatusProps {
  summary?: AdaptiveWeeklyOverdraftSummary;
}

const workspaceStopReasons = new Set(["deactivated_workspace", "account_deactivated", "workspace_deactivated"]);
const authenticationStopReasons = new Set(["authentication_failed", "token_revoked", "invalid_credentials"]);

export function AdaptiveWeeklyOverdraftStatus({ summary }: AdaptiveWeeklyOverdraftStatusProps) {
  const { locale, tx, formatNumber } = useI18n();
  if (!summary || summary.phase === "idle") return null;

  if (summary.phase === "hard_stopped") {
    const reason = String(summary.hard_stop_reason || "").trim().toLowerCase();
    const label = workspaceStopReasons.has(reason)
      ? tx("ui.adaptive_overdraft_workspace_stopped")
      : authenticationStopReasons.has(reason)
        ? tx("ui.adaptive_overdraft_authentication_stopped")
        : tx("ui.adaptive_overdraft_stopped");
    return (
      <div className="adaptive-overdraft-status is-stopped" role="status">
        <strong>{label}</strong><span>{tx("ui.adaptive_overdraft_no_further_probes")}</span>
      </div>
    );
  }

  const strategy = summary.strategy?.toUpperCase();
  const label = summary.phase === "exhausted"
    ? tx("ui.adaptive_overdraft_exhausted")
    : strategy
      ? tx("ui.adaptive_overdraft_strategy", { strategy })
      : tx("ui.adaptive_overdraft_armed");
  return (
    <div className="adaptive-overdraft-status" role="status">
      <strong>{label}</strong>
      <span>{tx("ui.adaptive_overdraft_success_count", { count: formatNumber(summary.post_threshold_successes) })}</span>
      <span>{tx("ui.adaptive_overdraft_token_count", { count: formatCompactNumber(summary.post_threshold_tokens, locale) })}</span>
    </div>
  );
}

function formatCompactNumber(value: number, locale: Locale): string {
  const normalized = Number.isFinite(value) ? Math.max(0, value) : 0;
  return new Intl.NumberFormat(localeFormats[locale].dateTimeLocale, {
    notation: normalized >= 1000 ? "compact" : "standard",
    maximumFractionDigits: 1,
  }).format(normalized);
}
