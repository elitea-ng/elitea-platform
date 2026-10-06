import { act, renderHook as renderTestingHook, waitFor, type RenderHookOptions } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';
import { usePipelineTestConversation, resetPipelineTestConversationsForTests, type PipelineTestChatIdentity } from './usePipelineTestConversation';
const api={create:vi.fn<(...args:unknown[])=>Promise<Record<string,unknown>>>(),details:vi.fn<(...args:unknown[])=>Promise<Record<string,unknown>>>()};
function renderHook<Result,Props>(callback:(props:Props)=>Result,options?:RenderHookOptions<Props>) {
 const client=new QueryClient({defaultOptions:{queries:{retry:false},mutations:{retry:false}}});
 return renderTestingHook(callback,{...options,wrapper:({children})=><QueryClientProvider client={client}>{children}</QueryClientProvider>});
}
const identity: PipelineTestChatIdentity = {
  projectId: "7",
  applicationId: "12",
  versionId: "19",
  userId: "41",
  pipelineName: "Pipeline",
  agentType: "pipeline",
};
function detail(version = "19", actor = "41") {
  return {
    id: "10",
    uuid: "c79eb6f8-7344-4b57-a9d0-585c28f33fa8",
    name: "Test",
    source: "editor_test",
    is_private: true,
    meta: {
      is_hidden: true,
      editor_test: {
        revision: 1,
        actor_id: actor,
        project_id: "7",
        application_id: "12",
        application_version_id: version,
      },
    },
    participants: [
      {
        id: "9",
        entity_name: "application",
        entity_meta: { id: "12", project_id: "7" },
        entity_settings: { version_id: version },
      },
    ],
    editor_test_runs: { rows: [], limit: 50, offset: 0, has_more: false },
    message_groups: [],
  };
}
function deferred<T>() {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>((done) => {
    resolve = done;
  });
  return { promise, resolve };
}
beforeEach(() => {
  resetPipelineTestConversationsForTests();
  configureGeneratedClient({baseUrl:'/api/v2'});
  server.use(
   http.post('/api/v2/elitea_core/conversations/prompt_lib/7',async({request})=>HttpResponse.json(await api.create(await request.json()))),
   http.get('/api/v2/elitea_core/conversation/prompt_lib/7/:id',async({request,params})=>{
    const query=new URL(request.url).searchParams;
    const input={projectId:'7',id:String(params['id']),editor_test_runs:query.get('editor_test_runs')==='true',runs_limit:Number(query.get('runs_limit'))};
    return HttpResponse.json(await api.details(input,request.signal));
   }),
  );
  api.create.mockReset();
  api.details.mockReset();
  api.create.mockResolvedValue(detail());
});
afterEach(()=>resetGeneratedClient());
describe("immutable editor Test lifecycle", () => {
  it("creates atomic hidden Test context once and derives user server-side", async () => {
    const hook = renderHook(() => usePipelineTestConversation(identity));
    act(() => {
      hook.result.current.ensure();
      hook.result.current.ensure();
    });
    await waitFor(() =>
      expect(hook.result.current.conversation?.id).toBe("10"),
    );
    expect(api.create).toHaveBeenCalledTimes(1);
    expect(api.details).not.toHaveBeenCalled();
    expect(api.create.mock.calls[0]?.[0]).toMatchObject({
      source: "editor_test",
      is_private: true,
      participants: [{ entity_name: "application" }],
    });
  });
  it("allocates a new immutable context for a saved version without rewriting the old participant", async () => {
    const hook = renderHook(({ input }) => usePipelineTestConversation(input), {
      initialProps: { input: identity },
    });
    act(() => hook.result.current.ensure());
    await waitFor(() => expect(hook.result.current.conversation).toBeDefined());
    hook.rerender({ input: { ...identity, versionId: "20" } });
    api.create.mockResolvedValue(detail("20"));
    act(() => hook.result.current.ensure());
    await waitFor(() => expect(api.create).toHaveBeenCalledTimes(2));
    await waitFor(() =>
      expect(hook.result.current.conversation?.participants?.[0]).toMatchObject(
        { entity_settings: { version_id: "20" } },
      ),
    );
  });
  it("refuses older Main responses without typed context and never enables ordinary Test execution", async () => {
    api.create.mockResolvedValue({
      id: "10",
      uuid: "ordinary",
      name: "Test",
      participants: [],
    });
    const hook = renderHook(() => usePipelineTestConversation(identity));
    act(() => hook.result.current.ensure());
    await waitFor(() => expect(hook.result.current.hasFailed).toBe(true));
    expect(hook.result.current.conversation).toBeUndefined();
  });
  it("drops stale bootstrap after actor changes", async () => {
    const first = deferred<ReturnType<typeof detail>>();
    api.create.mockReturnValueOnce(first.promise);
    const hook = renderHook(({ input }) => usePipelineTestConversation(input), {
      initialProps: { input: identity },
    });
    act(() => hook.result.current.ensure());
    hook.rerender({ input: { ...identity, userId: "42" } });
    await act(async () => {first.resolve(detail());await Promise.resolve();});
    expect(hook.result.current.conversation).toBeUndefined();
  });
  it("restores existing saved identity without creation and survives completion acknowledgement", async () => {
    const onComplete = vi.fn();
    api.details.mockResolvedValue(detail("18"));
    const initialProps: {id:string|undefined} = {id:"10"};
    const hook = renderHook(
      ({ id }: {id:string|undefined}) =>
        usePipelineTestConversation(identity, {
          conversationId: id,
          onComplete,
        }),
      { initialProps },
    );
    act(() => hook.result.current.ensure());
    await waitFor(() => expect(onComplete).toHaveBeenCalledTimes(1));
    expect(api.create).not.toHaveBeenCalled();
    expect(api.details).toHaveBeenCalledWith(
      expect.objectContaining({
        id: "10",
        editor_test_runs: true,
        runs_limit: 50,
      }),
      expect.any(AbortSignal),
    );
    hook.rerender({ id: undefined });
    expect(hook.result.current.conversation?.id).toBe("10");
  });
  it("drops stale restore and refuses foreign actor context", async () => {
    const first = deferred<ReturnType<typeof detail>>();
    api.details
      .mockReturnValueOnce(first.promise)
      .mockResolvedValueOnce(detail("19", "99"));
    const complete = vi.fn();
    const hook = renderHook(
      ({ id }: {id:string|undefined}) =>
        usePipelineTestConversation(identity, {
          conversationId: id,
          onComplete: complete,
        }),
      { initialProps: { id: "10" } },
    );
    hook.rerender({ id: "11" });
    await act(async () => {first.resolve(detail());await Promise.resolve();});
    await waitFor(() => expect(hook.result.current.hasFailed).toBe(true));
    expect(complete).not.toHaveBeenCalled();
    expect(api.create).not.toHaveBeenCalled();
  });
});

it("selects original paused response and question without restoring other run decisions", async () => {
  const firstResponse = "bd5ef16f-78a7-4c6e-9f55-4d94e4a4eefe";
  const firstQuestion = "734326c5-7f3b-42a5-b82f-0a3bfd7326ce";
  const secondResponse = "8b0ee0d6-e186-4301-b363-61dfc880eaa0";
  const secondQuestion = "839b2ab1-ae03-48c3-9330-d67d238c81a0";
  const run = (response: string, question: string) => ({
    response_message_id: response,
    question_id: question,
    execution_id: response,
    execution_generation: "original-generation",
    phase: "PAUSED",
    state: "COMPLETED",
    desired_state: "RUNNING",
    admitted_at: "2026-10-02T00:00:00Z",
    settled_at: "2026-10-02T00:01:00Z",
    input_reference: {
      bundle_id: "admitted",
      entry_id: "request",
      immutable_version: "1",
      content_digest: "a".repeat(64),
    },
    can_control: true,
  });
  const snapshot = {
    ...detail(),
    editor_test_runs: {
      rows: [
        run(firstResponse, firstQuestion),
        run(secondResponse, secondQuestion),
      ],
      limit: 50,
      offset: 0,
      has_more: false,
    },
    message_groups: [
      { uuid: firstQuestion, meta: {} },
      {
        uuid: firstResponse,
        meta: {
          execution_generation: "original-generation",
          thread_id: "original-thread",
          hitl_interrupts: [
            {
              interrupt_id: "original-interrupt",
              tool_call_id: "original-tool",
            },
          ],
        },
      },
      { uuid: secondQuestion, meta: {} },
      {
        uuid: secondResponse,
        meta: { hitl_interrupts: [{ interrupt_id: "other-interrupt" }] },
      },
    ],
  };
  api.details.mockResolvedValue(snapshot);
  const hook = renderHook(() =>
    usePipelineTestConversation(identity, {
      conversationId: "10",
      onComplete: vi.fn(),
    }),
  );
  await waitFor(() =>
    expect(hook.result.current.selectedRun?.response_message_id).toBe(
      firstResponse,
    ),
  );
  expect(hook.result.current.conversation?.message_groups).toEqual(
    snapshot.message_groups.slice(0, 2),
  );
  expect(hook.result.current.conversation?.isPlayback).not.toBe(true);
  act(() => hook.result.current.selectRun(secondResponse));
  expect(hook.result.current.conversation?.message_groups).toEqual(
    snapshot.message_groups.slice(2),
  );
  expect(api.create).not.toHaveBeenCalled();
});

it("reads existing runs when a page-local context remounts without creating another context", async () => {
  const first = renderHook(() => usePipelineTestConversation(identity));
  act(() => first.result.current.ensure());
  await waitFor(() => expect(first.result.current.conversation).toBeDefined());
  first.unmount();
  api.details.mockResolvedValue(detail());
  const second = renderHook(() => usePipelineTestConversation(identity));
  act(() => second.result.current.ensure());
  await waitFor(() => expect(second.result.current.conversation).toBeDefined());
  expect(api.create).toHaveBeenCalledTimes(1);
  expect(api.details).toHaveBeenCalledTimes(1);
});


it('releases only the local Test pointer after Clear and permits a fresh immutable context',async()=>{
 const hook=renderHook(()=>usePipelineTestConversation(identity));act(()=>hook.result.current.ensure());
 await waitFor(()=>expect(hook.result.current.conversation).toBeDefined());
 act(()=>hook.result.current.release());expect(hook.result.current.conversation).toBeUndefined();
 act(()=>hook.result.current.ensure());await waitFor(()=>expect(api.create).toHaveBeenCalledTimes(2));
 expect(api.details).not.toHaveBeenCalled();
});
