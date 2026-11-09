import assert from 'node:assert/strict';
import { describe, it } from 'node:test';

import { MAX_SUPPORT_CONTACT_CHARS, effectiveSupportContact, normalizeSupportContact, supportContactLink } from './index';

describe('normalizeSupportContact', () => {
  it('trims and keeps one bounded line', () => {
    assert.equal(normalizeSupportContact('  IT Service Desk  '), 'IT Service Desk');
    assert.equal(normalizeSupportContact(''), null);
    assert.equal(normalizeSupportContact('   '), null);
    assert.equal(normalizeSupportContact(5), null);
    assert.equal(normalizeSupportContact(null), null);
    assert.equal(normalizeSupportContact('IT\nDesk'), null);
    assert.equal(normalizeSupportContact('IT\u202eksed'), null);
    assert.equal(normalizeSupportContact('IT \u2066desk'), null);
    // The bound is in characters, as the server's is.
    assert.equal(normalizeSupportContact('é'.repeat(MAX_SUPPORT_CONTACT_CHARS)), 'é'.repeat(MAX_SUPPORT_CONTACT_CHARS));
    assert.equal(normalizeSupportContact('é'.repeat(MAX_SUPPORT_CONTACT_CHARS + 1)), null);
  });
});

describe('effectiveSupportContact', () => {
  it('managed beats workspace, and an invalid managed value does not hide the workspace one', () => {
    assert.equal(effectiveSupportContact('Managed desk', 'Workspace desk'), 'Managed desk');
    assert.equal(effectiveSupportContact(null, 'Workspace desk'), 'Workspace desk');
    assert.equal(effectiveSupportContact('two\nlines', 'Workspace desk'), 'Workspace desk');
    assert.equal(effectiveSupportContact(undefined, ''), null);
  });
});

describe('supportContactLink', () => {
  it('links an https address first, then an email address, then a phone number', () => {
    assert.deepEqual(supportContactLink('Help: https://help.example.com/it, or help@example.com'), {
      text: 'Help: https://help.example.com/it, or help@example.com',
      href: 'https://help.example.com/it',
    });
    // A bracket the address opened is part of it; sentence punctuation after it is not.
    assert.equal(supportContactLink('Help (https://wiki.example.com/IT_(Support)).').href, 'https://wiki.example.com/IT_(Support)');
    assert.equal(supportContactLink('See https://help.example.com/u/@it').href, 'https://help.example.com/u/@it');
    assert.equal(supportContactLink('IT Service Desk, help@example.com').href, 'mailto:help@example.com');
    assert.equal(supportContactLink('IT Service Desk, +1 (555) 010-0100').href, 'tel:+15550100100');
    assert.equal(supportContactLink('Call 555 0100 22').href, 'tel:555010022');
    assert.equal(supportContactLink('Tel: (555) 010-0100').href, 'tel:5550100100');
  });

  it('makes no other link', () => {
    assert.equal(supportContactLink('Ask the IT desk on floor 3').href, null);
    assert.equal(supportContactLink('javascript:alert(1)').href, null);
    assert.equal(supportContactLink('http://help.example.com').href, null);
    assert.equal(supportContactLink('file:///etc/passwd').href, null);
    assert.equal(supportContactLink('Room 12').href, null);
    // Dates and opening hours are not phone numbers.
    assert.equal(supportContactLink('IT desk, open 08.00-17.00').href, null);
    assert.equal(supportContactLink('Service desk (since 2026-01-15)').href, null);
    // Credentials in an https address: not a link (the email of the userinfo is not either).
    assert.equal(supportContactLink('https://u:p@help.example.com').href, null);
  });
});
