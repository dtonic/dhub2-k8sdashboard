#!/usr/bin/env node
import { execFileSync } from "node:child_process";
import { readdirSync, readFileSync } from "node:fs";
import { join } from "node:path";

const EXPECTED_NPM = "12.0.2";
const EXPECTED = new Map([
  ["brace-expansion", "5.0.9"],
  ["ip-address", "10.3.1"],
  ["tar", "7.5.21"],
]);

const npmVersion = execFileSync("npm", ["--version"], { encoding: "utf8" }).trim();
if (npmVersion !== EXPECTED_NPM) throw new Error(`npm version drift: ${npmVersion}`);
execFileSync("npm", ["ls", "--global", "--all"], { stdio: "ignore" });
const globalRoot = execFileSync("npm", ["root", "--global"], { encoding: "utf8" }).trim();
const npmRoot = join(globalRoot, "npm");
const seen = new Map([...EXPECTED.keys()].map((name) => [name, 0]));

function visit(directory) {
  for (const entry of readdirSync(directory, { withFileTypes: true })) {
    if (!entry.isDirectory()) continue;
    const path = join(directory, entry.name);
    const packageFile = join(path, "package.json");
    try {
      const pkg = JSON.parse(readFileSync(packageFile, "utf8"));
      if (EXPECTED.has(pkg.name)) {
        const expected = EXPECTED.get(pkg.name);
        if (pkg.version !== expected) throw new Error(`${pkg.name} vulnerable copy remains: ${pkg.version}`);
        seen.set(pkg.name, seen.get(pkg.name) + 1);
      }
    } catch (error) {
      if (error?.code !== "ENOENT") throw error;
    }
    visit(path);
  }
}

visit(npmRoot);
for (const [name, count] of seen) {
  if (count === 0) throw new Error(`${name} fixed package was not found below ${npmRoot}`);
}
console.log(`npm toolchain: npm ${EXPECTED_NPM}, brace-expansion 5.0.9, ip-address 10.3.1, tar 7.5.21; vulnerable copies 0`);
