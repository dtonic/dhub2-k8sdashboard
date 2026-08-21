#!/usr/bin/env node
/**
 * build-previews.mjs
 * --------------------------------------------------------------------------
 * design-system/**\/*.preview.html 을 자기완결(self-contained) HTML로 빌드하여
 * design-system/dist/ 에 출력합니다. Claude Design(design-sync)에는 dist/ 만
 * 업로드합니다.
 *
 * 규칙
 * - 소스 preview의 첫 줄은 반드시 `<!-- @dsCard group="..." -->` 마커입니다.
 *   빌드 결과에서도 첫 줄로 유지됩니다. (Design System 패널이 이 마커로
 *   카드 인덱스를 만듭니다.)
 * - <style> 안의 `/* @inline: <path> *\/` 지시자를 design-system 루트 기준
 *   경로의 CSS로 치환합니다. CSS의 @import 도 재귀적으로 인라인합니다.
 * - 외부 리소스(script/link/font/이미지 URL)는 허용하지 않습니다. 빌드 시 검사합니다.
 *
 * 사용: node design-system/scripts/build-previews.mjs [--check]
 *   --check : 파일을 쓰지 않고 검증만 수행 (CI용)
 */

import { readFile, writeFile, mkdir, readdir, rm } from "node:fs/promises";
import { existsSync } from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

const ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
const DIST = path.join(ROOT, "dist");
const CHECK_ONLY = process.argv.includes("--check");

const INLINE_RE = /^[ \t]*\/\*\s*@inline:\s*(.+?)\s*\*\/[ \t]*$/gm;
const IMPORT_RE = /^[ \t]*@import\s+["'](.+?)["']\s*;[ \t]*$/gm;
const DS_CARD_RE = /^<!--\s*@dsCard\s+group="([^"]+)"(?:\s+name="([^"]*)")?(?:\s+viewport="(\d+)(?:x(\d+))?")?\s*-->$/;
const EXTERNAL_RE = /(?:src|href)\s*=\s*["'](https?:)?\/\//i;

async function walk(dir, out = []) {
  for (const entry of await readdir(dir, { withFileTypes: true })) {
    if (entry.name === "dist" || entry.name === "node_modules") continue;
    const full = path.join(dir, entry.name);
    if (entry.isDirectory()) await walk(full, out);
    else if (entry.name.endsWith(".preview.html")) out.push(full);
  }
  return out;
}

async function inlineCss(cssPath, seen = new Set()) {
  const abs = path.resolve(cssPath);
  if (seen.has(abs)) return "";
  seen.add(abs);
  if (!existsSync(abs)) throw new Error(`CSS not found: ${path.relative(ROOT, abs)}`);
  const css = await readFile(abs, "utf8");
  const parts = [];
  let last = 0;
  for (const m of css.matchAll(IMPORT_RE)) {
    parts.push(css.slice(last, m.index));
    parts.push(await inlineCss(path.resolve(path.dirname(abs), m[1]), seen));
    last = m.index + m[0].length;
  }
  parts.push(css.slice(last));
  return parts.join("");
}

async function build(file) {
  const rel = path.relative(ROOT, file);
  const src = await readFile(file, "utf8");
  const lines = src.split("\n");

  const card = DS_CARD_RE.exec(lines[0].trim());
  if (!card) throw new Error(`${rel}: 첫 줄에 <!-- @dsCard group="..." --> 마커가 없습니다.`);

  const seen = new Set();
  let out = "";
  let last = 0;
  for (const m of src.matchAll(INLINE_RE)) {
    out += src.slice(last, m.index);
    out += (await inlineCss(path.resolve(ROOT, m[1]), seen)).trimEnd();
    last = m.index + m[0].length;
  }
  out += src.slice(last);

  if (EXTERNAL_RE.test(out)) throw new Error(`${rel}: 외부 리소스 참조가 있습니다. preview는 자기완결이어야 합니다.`);
  if (!out.startsWith("<!--")) throw new Error(`${rel}: 빌드 결과의 첫 줄이 @dsCard 마커가 아닙니다.`);

  return { rel, group: card[1], html: out };
}

const files = (await walk(ROOT)).sort();
if (files.length === 0) {
  console.error("preview 파일을 찾지 못했습니다.");
  process.exit(1);
}

const built = [];
for (const f of files) built.push(await build(f));

if (!CHECK_ONLY) {
  await rm(DIST, { recursive: true, force: true });
  for (const b of built) {
    const dest = path.join(DIST, b.rel);
    await mkdir(path.dirname(dest), { recursive: true });
    await writeFile(dest, b.html, "utf8");
  }
}

const byGroup = built.reduce((acc, b) => ((acc[b.group] ??= []).push(b.rel), acc), {});
for (const [group, items] of Object.entries(byGroup)) {
  console.log(`${group}:`);
  for (const i of items) console.log(`  ${i}`);
}
console.log(`\n${built.length} preview${CHECK_ONLY ? " 검증 완료" : ` → ${path.relative(process.cwd(), DIST)}`}`);
