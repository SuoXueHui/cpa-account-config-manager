import { render, screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { AdaptiveWeeklyOverdraftStatus } from "./AdaptiveWeeklyOverdraftStatus";

describe("AdaptiveWeeklyOverdraftStatus", () => {
  it("shows the active strategy with post-threshold success and token counters", () => {
    render(<AdaptiveWeeklyOverdraftStatus summary={{
      phase: "active_s2",
      strategy: "s2",
      post_threshold_successes: 12,
      post_threshold_tokens: 12_345,
    }} />);

    const status = screen.getByRole("status");
    expect(status).toHaveTextContent("Adaptive S2");
    expect(status).toHaveTextContent("观测窗口成功 12");
    expect(status).toHaveTextContent("1.2万 tok");
  });

  it("shows injected strategy lifecycle counters", () => {
    render(<AdaptiveWeeklyOverdraftStatus summary={{
      phase: "active_s1",
      strategy: "s1",
      strategy_stats: { s1: { attempts: 2, successes: 1, failures: 1 } },
      post_threshold_successes: 1,
      post_threshold_tokens: 512,
    }} />);

    expect(screen.getByRole("status")).toHaveTextContent("注入 S1：2 次 / 成功 1 / 失败 1");
  });

  it("maps a deactivated workspace hard stop to fixed operator text", () => {
    render(<AdaptiveWeeklyOverdraftStatus summary={{
      phase: "hard_stopped",
      post_threshold_successes: 0,
      post_threshold_tokens: 0,
      hard_stop_reason: "deactivated_workspace",
    }} />);

    expect(screen.getByRole("status")).toHaveTextContent("工作区已停止 · 不再探测");
  });

  it("renders nothing for idle or missing summaries", () => {
    const { rerender } = render(<AdaptiveWeeklyOverdraftStatus />);
    expect(screen.queryByRole("status")).not.toBeInTheDocument();

    rerender(<AdaptiveWeeklyOverdraftStatus summary={{
      phase: "idle",
      post_threshold_successes: 0,
      post_threshold_tokens: 0,
    }} />);
    expect(screen.queryByRole("status")).not.toBeInTheDocument();
  });
});
