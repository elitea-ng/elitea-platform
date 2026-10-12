import { readFileSync } from "fs";

// Parses a line of "key=value".
export function parseLine(line: string): [string, string] {
  const [key, value] = line.split("=");
  return [key.trim(), value.trim()];
}

export class Config {
  private values = new Map<string, string>();

  load(path: string): void {
    for (const line of readFileSync(path, "utf8").split("\n")) {
      const [key, value] = parseLine(line);
      this.values.set(key, value);
    }
  }

  get(key: string): string | undefined {
    return this.values.get(key);
  }
}
