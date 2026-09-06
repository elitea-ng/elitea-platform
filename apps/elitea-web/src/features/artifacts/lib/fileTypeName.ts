/**
 * Extension → human file-type name, the `Type` column of the artifacts table.
 *
 * Ported from `apps/elitea-ui/src/utils/fileTypes.js`'s `getFileTypeName`,
 * including its fallback (`EXT` upper-cased, or `Unknown` for a name with no
 * extension). The baseline's map is kept whole rather than trimmed to the
 * extensions this app has seen: the column is a plain lookup, and a missing
 * entry degrades silently to the upper-cased extension, which is exactly the
 * kind of "looks fine, is wrong" difference the parity pass exists to remove.
 */
const TYPE_NAMES: Readonly<Record<string, string>> = {
  txt: 'Text', text: 'Text', md: 'Markdown', markdown: 'Markdown', json: 'JSON',
  js: 'JavaScript', jsx: 'JavaScript (JSX)', javascript: 'JavaScript',
  ts: 'TypeScript', tsx: 'TypeScript (TSX)', typescript: 'TypeScript',
  py: 'Python', python: 'Python', java: 'Java',
  c: 'C', cpp: 'C++', cxx: 'C++', cc: 'C++', h: 'C Header', hpp: 'C++ Header',
  cs: 'C#', csharp: 'C#', php: 'PHP', rb: 'Ruby', ruby: 'Ruby', go: 'Go',
  rs: 'Rust', rust: 'Rust', kt: 'Kotlin', kotlin: 'Kotlin', swift: 'Swift',
  r: 'R', scala: 'Scala', sh: 'Shell Script', bash: 'Bash Script',
  zsh: 'Zsh Script', ps1: 'PowerShell', powershell: 'PowerShell',
  html: 'HTML', htm: 'HTML', css: 'CSS', scss: 'SCSS', sass: 'Sass',
  less: 'Less', xml: 'XML', svg: 'SVG',
  yaml: 'YAML', yml: 'YAML', toml: 'TOML', ini: 'INI', conf: 'Configuration',
  config: 'Configuration', env: 'Environment', properties: 'Properties',
  rst: 'reStructuredText', tex: 'LaTeX',
  csv: 'CSV', tsv: 'TSV', sql: 'SQL', feature: 'Feature File',
  gherkin: 'Gherkin', patch: 'Patch File', diff: 'Diff File',
  zip: 'ZIP Archive', tar: 'TAR Archive', gz: 'Gzip Archive',
  rar: 'RAR Archive', '7z': '7-Zip Archive',
  jpg: 'JPEG Image', jpeg: 'JPEG Image', png: 'PNG Image', gif: 'GIF Image',
  bmp: 'BMP Image', webp: 'WebP Image', ico: 'Icon',
  pdf: 'PDF Document', doc: 'Word Document', docx: 'Word Document',
  xls: 'Excel Spreadsheet', xlsx: 'Excel Spreadsheet', ppt: 'PowerPoint',
  pptx: 'PowerPoint',
  mp3: 'MP3 Audio', wav: 'WAV Audio', flac: 'FLAC Audio', ogg: 'OGG Audio',
  mp4: 'MP4 Video', avi: 'AVI Video', mkv: 'MKV Video', mov: 'QuickTime Video',
  wmv: 'WMV Video',
  dockerfile: 'Dockerfile', makefile: 'Makefile', gitignore: 'Git Ignore',
  editorconfig: 'Editor Config', eslintrc: 'ESLint Config',
  prettierrc: 'Prettier Config', babelrc: 'Babel Config',
  log: 'Log File', logs: 'Log File',
  pl: 'Perl', perl: 'Perl', lua: 'Lua', vim: 'Vim Script', dart: 'Dart',
  jsonl: 'JSON Lines', ndjson: 'Newline Delimited JSON',
  gradle: 'Gradle Build', mvn: 'Maven', pom: 'Maven POM', cmake: 'CMake',
  license: 'License', licence: 'License', copyright: 'Copyright',
  changelog: 'Changelog', readme: 'README',
  cfg: 'Config File', cnf: 'Config File', rc: 'RC File', profile: 'Profile',
  bashrc: 'Bash RC', zshrc: 'Zsh RC', vimrc: 'Vim RC',
  gitconfig: 'Git Config', npmrc: 'NPM Config', yarnrc: 'Yarn Config',
  adoc: 'AsciiDoc', asciidoc: 'AsciiDoc', org: 'Org Mode', wiki: 'Wiki',
  cql: 'Cassandra Query Language', hql: 'Hive Query Language',
  psql: 'PostgreSQL',
  rdf: 'RDF', owl: 'OWL Ontology', n3: 'Notation3', ttl: 'Turtle',
  sparql: 'SPARQL',
  proto: 'Protocol Buffer', protobuf: 'Protocol Buffer', thrift: 'Apache Thrift',
  avsc: 'Avro Schema', avro: 'Avro',
  dockerignore: 'Docker Ignore', k8s: 'Kubernetes', kube: 'Kubernetes',
  helm: 'Helm Chart',
  jenkins: 'Jenkins', jenkinsfile: 'Jenkinsfile', travis: 'Travis CI',
  circleci: 'CircleCI', github: 'GitHub Actions', 'gitlab-ci': 'GitLab CI',
  wat: 'WebAssembly Text', wast: 'WebAssembly Script',
  xsd: 'XML Schema', wsdl: 'WSDL',
  mermaid: 'Mermaid Diagram', mmd: 'Mermaid Diagram', dot: 'Graphviz',
  gv: 'Graphviz', puml: 'PlantUML', plantuml: 'PlantUML',
};

/** Lower-cased extension without the dot; `''` when the name carries none. */
function extensionOf(filename: string): string {
  const dot = filename.lastIndexOf('.');
  if (dot <= 0 || dot === filename.length - 1) return '';
  return filename.slice(dot + 1).toLowerCase();
}

export function getFileTypeName(filename: string): string {
  const extension = extensionOf(filename);
  return TYPE_NAMES[extension] ?? (extension === '' ? 'Unknown' : extension.toUpperCase());
}
