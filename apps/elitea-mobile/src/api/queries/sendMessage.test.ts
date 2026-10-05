import {
  MAX_MENTION_IDS,
  TurnInputError,
  prepareTurn,
  restorePreparedTurn,
  type TurnRequest,
} from '@/api/queries/sendMessage';

const UUID = '6f1c2b9e-0a7d-4c3e-9b1a-2d4e6f8a0b1c';
const Q = '0b7e5d3a-1c2f-4e6a-8b9d-0a1b2c3d4e5f';

function request(overrides: Partial<TurnRequest> = {}): TurnRequest {
  return { projectId: 7, conversationUuid: UUID, participantId: 3, questionId: Q, userInput: 'hello', ...overrides };
}

const reasonOf = (fn: () => unknown) => {
  try {
    fn();
  } catch (error) {
    return error instanceof TurnInputError ? error.reason : 'other';
  }
  return null;
};

describe('prepareTurn body bytes (ChatSendRequest)', () => {
  it('a plain send carries neither mentions nor attachments', () => {
    expect(prepareTurn(request()).bodyJson).toBe(
      `{"payload":{"user_input":"hello"},"project_id":7,"participant_id":3,"conversation_uuid":"${UUID}","question_id":"${Q}"}`,
    );
  });

  it('adds user_ids (deduped, in order) and is_mentioning_everyone only when true', () => {
    expect(prepareTurn(request({ mentions: { userIds: [42, 9, 42], everyone: false } })).bodyJson).toBe(
      `{"payload":{"user_input":"hello"},"project_id":7,"participant_id":3,"conversation_uuid":"${UUID}","question_id":"${Q}","user_ids":[42,9]}`,
    );
    expect(prepareTurn(request({ mentions: { userIds: [], everyone: true } })).bodyJson).toBe(
      `{"payload":{"user_input":"hello"},"project_id":7,"participant_id":3,"conversation_uuid":"${UUID}","question_id":"${Q}","is_mentioning_everyone":true}`,
    );
    expect(prepareTurn(request({ mentions: { userIds: [], everyone: false } })).bodyJson).toBe(prepareTurn(request()).bodyJson);
  });

  it('adds payload.attachments with the upload filepaths', () => {
    const attachments = [
      { filepath: `/chat-attachments/${UUID}/photo-a1b2.jpg`, name: 'photo.jpg' },
      { filepath: `/chat-attachments/${UUID}/report-c3d4.pdf`, name: 'report.pdf' },
    ];
    expect(prepareTurn(request({ attachments })).bodyJson).toBe(
      `{"payload":{"user_input":"hello","attachments":[` +
        `{"filepath":"/chat-attachments/${UUID}/photo-a1b2.jpg","name":"photo.jpg"},` +
        `{"filepath":"/chat-attachments/${UUID}/report-c3d4.pdf","name":"report.pdf"}]},` +
        `"project_id":7,"participant_id":3,"conversation_uuid":"${UUID}","question_id":"${Q}"}`,
    );
  });

  it('keeps mentions and attachments on a model (adhoc) turn too', () => {
    const body = JSON.parse(
      prepareTurn(
        request({
          participantId: 0,
          model: { name: 'gpt-5', projectId: null },
          mentions: { userIds: [5], everyone: true },
          attachments: [{ filepath: `/b/${UUID}/x.md`, name: 'x.md' }],
        }),
      ).bodyJson,
    );
    expect(body).toMatchObject({
      user_ids: [5],
      is_mentioning_everyone: true,
      payload: { attachments: [{ filepath: `/b/${UUID}/x.md` }] },
      llm_settings: { model_name: 'gpt-5', stream: true },
    });
  });
});

describe('prepareTurn refusals', () => {
  it('refuses a string, zero, negative or fractional mention id', () => {
    for (const id of ['42', 0, -1, 1.5] as unknown[]) {
      expect(reasonOf(() => prepareTurn(request({ mentions: { userIds: [id as number], everyone: false } })))).toBe('mentions');
    }
  });

  it('refuses more than 64 distinct ids, and accepts exactly 64', () => {
    const ids = Array.from({ length: MAX_MENTION_IDS }, (_, i) => i + 1);
    expect(reasonOf(() => prepareTurn(request({ mentions: { userIds: ids, everyone: false } })))).toBeNull();
    expect(reasonOf(() => prepareTurn(request({ mentions: { userIds: [...ids, 65], everyone: false } })))).toBe('mentions');
  });

  it.each([
    ['another conversation', `/chat-attachments/11111111-2222-4333-8444-555555555555/a.txt`],
    ['no bucket', `/${UUID}/a.txt`],
    ['a nested name', `/chat-attachments/${UUID}/dir/a.txt`],
    ['a dot-dot name', `/chat-attachments/${UUID}/..`],
    ['a relative path', `chat-attachments/${UUID}/a.txt`],
  ])('refuses a filepath with %s', (_label, filepath) => {
    expect(reasonOf(() => prepareTurn(request({ attachments: [{ filepath, name: 'a.txt' }] })))).toBe('attachments');
  });

  it('refuses an attachment without a display name', () => {
    expect(reasonOf(() => prepareTurn(request({ attachments: [{ filepath: `/b/${UUID}/a.txt`, name: ' ' }] })))).toBe(
      'attachments',
    );
  });
});

describe('restorePreparedTurn with mentions and attachments', () => {
  it('re-sends the stored bytes unchanged and reads the fields back', () => {
    const prepared = prepareTurn(
      request({
        mentions: { userIds: [42], everyone: true },
        attachments: [{ filepath: `/chat-attachments/${UUID}/a-1f2e.png`, name: 'a.png' }],
      }),
    );
    const restored = restorePreparedTurn(prepared.bodyJson);
    expect(restored.bodyJson).toBe(prepared.bodyJson);
    expect(restored.request.mentions).toEqual({ userIds: [42], everyone: true });
    expect(restored.request.attachments).toEqual([{ filepath: `/chat-attachments/${UUID}/a-1f2e.png`, name: 'a.png' }]);
    // A retry after restore prepares to the identical bytes.
    expect(prepareTurn(restored.request).bodyJson).toBe(prepared.bodyJson);
  });

  it('refuses a stored body whose attachment points at another conversation', () => {
    const tampered = prepareTurn(request({ attachments: [{ filepath: `/b/${UUID}/a.txt`, name: 'a.txt' }] })).bodyJson.replace(
      `/b/${UUID}/`,
      '/b/11111111-2222-4333-8444-555555555555/',
    );
    expect(() => restorePreparedTurn(tampered)).toThrow(TurnInputError);
  });
});
