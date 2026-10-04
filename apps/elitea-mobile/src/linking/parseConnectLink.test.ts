import { parseConnectLink, type ParseConnectLinkOptions } from './parseConnectLink';

// Mirrors identity.connectSchemes from app.config.ts; passed explicitly so the
// parser stays a pure function under test.
const opts: ParseConnectLinkOptions = { connectSchemes: ['zefir', 'agentzefir'] };
const dev: ParseConnectLinkOptions = { ...opts, allowPrivateHttp: true };

const ok = (origin: string) => ({ ok: true, origin });
const err = (error: string) => ({ ok: false, error });

describe('parseConnectLink', () => {
  it.each([
    ['zefir://connect?server=https%3A%2F%2Fai.example.com', 'https://ai.example.com'],
    ['zefir://connect?server=https://ai.example.com/', 'https://ai.example.com'],
    ['agentzefir://connect?server=https%3A%2F%2Fai.example.com%3A8443', 'https://ai.example.com:8443'],
    ['ZEFIR://Connect/?foo=1&server=https%3A%2F%2FAI.Example.COM#x', 'https://ai.example.com'],
    ['https://ai.example.com', 'https://ai.example.com'],
    ['  https://ai.example.com/app/chat?project=3#top  ', 'https://ai.example.com'],
    ['https://ai.example.com:443/', 'https://ai.example.com'],
    ['https://10.0.0.5:9443', 'https://10.0.0.5:9443'],
    ['ai.example.com', 'https://ai.example.com'],
    ['https://[2001:db8::1]:8443/x', 'https://[2001:db8::1]:8443'],
  ])('accepts %s', (input, origin) => {
    expect(parseConnectLink(input, opts)).toEqual(ok(origin));
  });

  it.each([
    ['', 'empty'],
    ['   ', 'empty'],
    ['ftp://ai.example.com', 'unsupported_scheme'],
    ['javascript://alert(1)', 'unsupported_scheme'],
    ['zefir://login?server=https%3A%2F%2Fai.example.com', 'not_connect_link'],
    ['zefir://connect', 'missing_server'],
    ['zefir://connect?server=', 'missing_server'],
    ['zefir://connect?server=https%3A%2F%2Fa.com&server=https%3A%2F%2Fb.com', 'ambiguous_server'],
    ['zefir://connect?server=zefir%3A%2F%2Fconnect%3Fserver%3Dx', 'nested_link'],
    ['https://user:pass@ai.example.com', 'credentials_in_url'],
    ['zefir://connect?server=https%3A%2F%2Fuser%40ai.example.com', 'credentials_in_url'],
    ['https://ai.example.com:0', 'invalid_port'],
    ['https://ai.example.com:70000', 'invalid_port'],
    ['https://ai_example.com', 'invalid_host'],
    ['https://300.1.1.1', 'invalid_host'],
    ['https://ai.example.com /x', 'malformed'],
    ['zefir://connect?server=%E0%A4%A', 'malformed'],
  ])('rejects %j with %s', (input, error) => {
    expect(parseConnectLink(input, opts)).toEqual(err(error));
  });

  describe('plain http', () => {
    it('is rejected by default, even for private hosts', () => {
      expect(parseConnectLink('http://localhost:8080', opts)).toEqual(err('insecure_http'));
      expect(parseConnectLink('http://192.168.1.10', opts)).toEqual(err('insecure_http'));
    });

    it.each([
      ['http://localhost:8080/api', 'http://localhost:8080'],
      ['http://127.0.0.1', 'http://127.0.0.1'],
      ['http://10.1.2.3:80', 'http://10.1.2.3'],
      ['http://192.168.29.60:8000', 'http://192.168.29.60:8000'],
      ['zefir://connect?server=http%3A%2F%2Flocalhost%3A3000', 'http://localhost:3000'],
    ])('allows private host %s when the dev flag is set', (input, origin) => {
      expect(parseConnectLink(input, dev)).toEqual(ok(origin));
    });

    it.each(['http://ai.example.com', 'http://172.16.0.1', 'http://8.8.8.8', 'http://192.169.0.1'])(
      'rejects public or unlisted host %s even with the dev flag',
      (input) => {
        expect(parseConnectLink(input, dev)).toEqual(err('insecure_http'));
      },
    );
  });
});
