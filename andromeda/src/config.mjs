// ~/.andromeda/config.json — where andromeda remembers your endpoint + token,
// so you `login` once and every run picks it up automatically (à la `hf auth
// login`). The token is a credential, so the file is written 0600 inside a 0700
// directory. Resolution order at run time is: flags > env > this config.

import { homedir } from "node:os";
import path from "node:path";
import { mkdirSync, readFileSync, writeFileSync, rmSync } from "node:fs";

const DIR = path.join(homedir(), ".andromeda");
export const configPath = path.join(DIR, "config.json");

export function loadConfig() {
  try {
    return JSON.parse(readFileSync(configPath, "utf8"));
  } catch {
    return {};
  }
}

export function saveConfig(cfg) {
  mkdirSync(DIR, { recursive: true, mode: 0o700 });
  writeFileSync(configPath, JSON.stringify(cfg, null, 2) + "\n", { mode: 0o600 });
}

export function clearConfig() {
  try {
    rmSync(configPath);
    return true;
  } catch {
    return false;
  }
}
