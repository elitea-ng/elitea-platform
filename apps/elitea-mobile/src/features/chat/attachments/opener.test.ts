import { tempFileName } from '@/features/chat/attachments/opener';

jest.mock('expo-file-system', () => ({ File: jest.fn(), Directory: jest.fn(), Paths: { cache: 'file:///cache' } }));
jest.mock('expo-crypto', () => ({ randomUUID: () => 'r' }));

describe('tempFileName', () => {
  it('keeps inert document and image types under their own extension', () => {
    expect(tempFileName('report-ab12.pdf')).toBe('report-ab12.pdf');
    expect(tempFileName('Plan.DOCX')).toBe('Plan.DOCX');
    expect(tempFileName('data.csv')).toBe('data.csv');
    expect(tempFileName('shot.png')).toBe('shot.png');
  });

  it('hands active types (HTML, SVG, XML, scripts) to the system viewer as plain text', () => {
    // The server serves these as octet-stream so no client renders them;
    // Quick Look / ACTION_VIEW pick the renderer from the extension.
    expect(tempFileName('report-ab12.html')).toBe('report-ab12.html.txt');
    expect(tempFileName('x.htm')).toBe('x.htm.txt');
    expect(tempFileName('logo.svg')).toBe('logo.svg.txt');
    expect(tempFileName('feed.xml')).toBe('feed.xml.txt');
    expect(tempFileName('run.js')).toBe('run.js.txt');
    expect(tempFileName('page.xhtml')).toBe('page.xhtml.txt');
    expect(tempFileName('noext')).toBe('noext.txt');
  });

  it('keeps the name one safe path segment', () => {
    expect(tempFileName('../../etc/passwd')).toBe('.._.._etc_passwd.txt');
    expect(tempFileName('')).toBe('file.txt');
    expect(tempFileName(`${'a'.repeat(200)}.pdf`)).toBe(`${'a'.repeat(116)}.pdf`);
  });
});
