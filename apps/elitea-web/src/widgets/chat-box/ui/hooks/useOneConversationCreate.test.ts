import { act, renderHook } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";

import { useOneConversationCreate } from "./useOneConversationCreate";

describe("useOneConversationCreate (#3077)", () => {
  it("creates one conversation for several sends in the same tick, and again for a new chat", async () => {
    let n = 0;
    const create = vi.fn(() => Promise.resolve({ uuid: `c${String((n += 1))}` }));
    const { result, rerender } = renderHook(({ uuid }) => useOneConversationCreate(create, uuid), { initialProps: { uuid: "" } });
    const created = await Promise.all([result.current("hi"), result.current("hi"), result.current("hi")]);
    expect(create).toHaveBeenCalledTimes(1);
    expect(created).toEqual([{ uuid: "c1" }, { uuid: "c1" }, { uuid: "c1" }]);
    // Before the page has the uuid, a later send still joins it.
    expect(await result.current("again")).toEqual({ uuid: "c1" });
    rerender({ uuid: "c1" });
    rerender({ uuid: "" });
    expect(await result.current("new chat")).toEqual({ uuid: "c2" });
  });

  it("forgets a failed create so the retry creates", async () => {
    const create = vi.fn<(question: string) => Promise<{ uuid: string } | undefined>>().mockRejectedValueOnce(new Error("offline")).mockResolvedValueOnce({ uuid: "c1" });
    const { result } = renderHook(() => useOneConversationCreate(create, ""));
    await expect(result.current("hi")).rejects.toThrow("offline");
    await act(() => Promise.resolve());
    expect(await result.current("hi")).toEqual({ uuid: "c1" });
  });
});
