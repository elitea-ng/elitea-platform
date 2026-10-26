import { credentialButtonTitle, credentialChoices, credentialFailure, credentialLabel } from '@/features/localWork/forgeCredential';

// The cloud thread's push and pull-request credential (contract 1.29): the same choices, in the
// same order, as the web and desktop pickers.

describe('credentialChoices', () => {
  it('lists per forge the project, its shared ones, then the owner own, each once', () => {
    const choices = credentialChoices([
      { type: 'github', project: { own: ['work', 'work'], shared: ['org'] }, personal: { own: ['mine'] } },
      { type: 'gitlab', project: { own: ['work'], shared: [] }, personal: undefined },
    ]);
    expect(choices.map((c) => c.key)).toEqual(['github:project:work', 'github:project:org', 'github:private:mine', 'gitlab:project:work']);
    expect(choices[2]).toMatchObject({ type: 'github', elitea_title: 'mine', private: true });
  });
});

describe('labels', () => {
  it('names the forge and whose it is', () => {
    expect(credentialLabel({ type: 'github', elitea_title: 'work', private: false })).toBe('work (GitHub)');
    expect(credentialLabel({ type: 'gitlab', elitea_title: 'mine', private: true })).toBe('mine (GitLab, yours)');
    expect(credentialButtonTitle(null)).toBe('Pushes and pull requests: choose a credential');
    expect(credentialButtonTitle({ type: 'gitlab', elitea_title: 'w', private: false })).toBe('Pushes and pull requests: w (GitLab)');
  });

  it('explains a refusal', () => {
    expect(credentialFailure(403)).toMatch(/turned off/);
    expect(credentialFailure(422)).toMatch(/not a GitHub or GitLab credential/);
    expect(credentialFailure(500)).toBe('The choice could not be saved. Try again.');
    expect(credentialFailure(undefined)).toBe('The choice could not be saved. Try again.');
  });
});
