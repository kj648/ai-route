#!/usr/bin/env node
// Checks that the web console is fully translated (no dependencies).
//
//  1. Every string literal passed as the first argument of t(...) in
//     web/app.js, web/account.js and web/presets.js, and every displayed Chinese string in the
//     preset catalog (names, notes, UA notes, categories, tag labels), has an
//     English entry in I18N_EN (web/i18n.js).
//  2. web/app.js and web/account.js have no Chinese text outside t(...)
//     arguments and comments
//     (the "中文" option of the alert language select is allowed).
//  3. Dictionary keys nobody uses are reported as a warning.
//
// usage: node scripts/check-i18n.mjs
import { readFileSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';
import vm from 'node:vm';

const root = join(dirname(fileURLToPath(import.meta.url)), '..');
const read = (f) => readFileSync(join(root, f), 'utf8');

// Han ideographs, CJK punctuation and fullwidth forms
const CJK = /[　-〿㐀-䶿一-鿿豈-﫿＀-￯]/;
const CJK_RUN = /[　-〿㐀-䶿一-鿿豈-﫿＀-￯]+/g;
const ALLOWED_RUNS = new Set(['中文']);

// ------------------------------------------------------------ dictionary
function loadDict() {
  const store = {};
  const ctx = {
    localStorage: { getItem: (k) => store[k] ?? null, setItem: (k, v) => { store[k] = String(v); } },
    navigator: { language: 'en-US' },
    document: { documentElement: {}, title: '' },
    location: { reload() {} },
  };
  vm.createContext(ctx);
  const dict = vm.runInContext(read('web/i18n.js') + '\n;I18N_EN', ctx, { filename: 'web/i18n.js' });
  if (!dict || typeof dict !== 'object') throw new Error('web/i18n.js: I18N_EN not found');
  return dict;
}

// ------------------------------------------------------------ lexer
// Tokenizes JavaScript well enough for this check: comments, strings,
// template literals (with nested ${...}), regex literals, identifiers and
// punctuation. Returns tokens with source offsets.
function lex(src, file) {
  const tokens = [];
  const comments = [];
  let i = 0;
  const n = src.length;
  // stack of brace depths for template substitutions
  const tmplStack = [];
  let braceDepth = 0;
  const isIdStart = (c) => /[A-Za-z_$]/.test(c);
  const isId = (c) => /[A-Za-z0-9_$]/.test(c);
  const lastSig = () => tokens[tokens.length - 1];
  const regexAllowed = () => {
    const tk = lastSig();
    if (!tk) return true;
    if (tk.type === 'num' || tk.type === 'str' || tk.type === 'tmpl' || tk.type === 'regex') return false;
    if (tk.type === 'id') return ['return', 'typeof', 'instanceof', 'in', 'of', 'new', 'delete', 'void', 'throw', 'case', 'do', 'else', 'yield', 'await'].includes(tk.value);
    if (tk.type === 'punct') return !(tk.value === ')' || tk.value === ']' || tk.value === '}');
    return true;
  };
  const lineOf = (pos) => src.slice(0, pos).split('\n').length;

  // reads a template literal chunk starting after ` or }, up to ` or ${
  const readTemplateChunk = (start) => {
    let j = start;
    let raw = '';
    while (j < n) {
      const c = src[j];
      if (c === '\\') { raw += src.slice(j, j + 2); j += 2; continue; }
      if (c === '`') return { end: j + 1, raw, closed: true };
      if (c === '$' && src[j + 1] === '{') return { end: j + 2, raw, closed: false };
      raw += c; j++;
    }
    throw new Error(`${file}: unterminated template literal at line ${lineOf(start)}`);
  };

  while (i < n) {
    const c = src[i];
    if (/\s/.test(c)) { i++; continue; }
    if (c === '/' && src[i + 1] === '/') {
      const e = src.indexOf('\n', i);
      const end = e < 0 ? n : e;
      comments.push([i, end]); i = end; continue;
    }
    if (c === '/' && src[i + 1] === '*') {
      const e = src.indexOf('*/', i + 2);
      if (e < 0) throw new Error(`${file}: unterminated comment`);
      comments.push([i, e + 2]); i = e + 2; continue;
    }
    if (c === '\'' || c === '"') {
      let j = i + 1;
      while (j < n && src[j] !== c) { if (src[j] === '\\') j++; if (src[j] === '\n') throw new Error(`${file}: unterminated string at line ${lineOf(i)}`); j++; }
      tokens.push({ type: 'str', start: i, end: j + 1, raw: src.slice(i, j + 1) });
      i = j + 1; continue;
    }
    if (c === '`') {
      const ch = readTemplateChunk(i + 1);
      if (ch.closed) tokens.push({ type: 'str', start: i, end: ch.end, raw: src.slice(i, ch.end) });
      else { tokens.push({ type: 'tmpl', start: i, end: ch.end, part: 'head' }); tmplStack.push(braceDepth); braceDepth++; }
      i = ch.end; continue;
    }
    if (c === '}' && tmplStack.length && braceDepth - 1 === tmplStack[tmplStack.length - 1]) {
      // end of a ${...} substitution: continue the template literal
      braceDepth--; tmplStack.pop();
      const ch = readTemplateChunk(i + 1);
      if (ch.closed) tokens.push({ type: 'tmpl', start: i, end: ch.end, part: 'tail' });
      else { tokens.push({ type: 'tmpl', start: i, end: ch.end, part: 'middle' }); tmplStack.push(braceDepth); braceDepth++; }
      i = ch.end; continue;
    }
    if (c === '/' && regexAllowed()) {
      let j = i + 1;
      let inClass = false;
      while (j < n) {
        const d = src[j];
        if (d === '\\') { j += 2; continue; }
        if (d === '\n') throw new Error(`${file}: unterminated regex at line ${lineOf(i)}`);
        if (inClass) { if (d === ']') inClass = false; } else if (d === '[') inClass = true; else if (d === '/') break;
        j++;
      }
      j++;
      while (j < n && /[a-z]/.test(src[j])) j++;
      tokens.push({ type: 'regex', start: i, end: j });
      i = j; continue;
    }
    if (/[0-9]/.test(c) || (c === '.' && /[0-9]/.test(src[i + 1]))) {
      let j = i + 1;
      while (j < n && /[0-9A-Za-z_.]/.test(src[j])) j++;
      tokens.push({ type: 'num', start: i, end: j });
      i = j; continue;
    }
    if (isIdStart(c)) {
      let j = i + 1;
      while (j < n && isId(src[j])) j++;
      tokens.push({ type: 'id', start: i, end: j, value: src.slice(i, j) });
      i = j; continue;
    }
    if (c === '{') braceDepth++;
    if (c === '}') braceDepth--;
    tokens.push({ type: 'punct', start: i, end: i + 1, value: c });
    i++;
  }
  return { tokens, comments, lineOf };
}

// string literal value of a 'str' token
const strValue = (tk) => vm.runInNewContext(tk.raw);

// first-argument literals of t(...) calls; template literals with ${} are errors
function tCalls(src, file) {
  const { tokens, comments, lineOf } = lex(src, file);
  const calls = [];
  const errors = [];
  for (let k = 0; k + 2 < tokens.length; k++) {
    const a = tokens[k], b = tokens[k + 1], c = tokens[k + 2];
    if (a.type !== 'id' || a.value !== 't' || b.type !== 'punct' || b.value !== '(') continue;
    const prev = tokens[k - 1];
    if (prev && prev.type === 'punct' && prev.value === '.') continue;
    if (prev && prev.type === 'id' && prev.value === 'function') continue;
    if (c.type === 'str') calls.push({ value: strValue(c), start: c.start, end: c.end, line: lineOf(c.start) });
    else if (c.type === 'tmpl') errors.push(`${file}:${lineOf(c.start)}: t() called with a template literal containing \${...}; use a {placeholder} instead`);
  }
  return { calls, errors, comments, tokens, lineOf };
}

// ------------------------------------------------------------ checks
const failures = [];
const dict = loadDict();
const used = new Set();
const missing = new Map(); // key -> first location

const need = (key, where) => {
  used.add(key);
  if (!Object.prototype.hasOwnProperty.call(dict, key) && !missing.has(key)) missing.set(key, where);
};

for (const file of ['web/app.js', 'web/account.js', 'web/presets.js']) {
  const src = read(file);
  const { calls, errors } = tCalls(src, file);
  failures.push(...errors);
  for (const c of calls) need(c.value, `${file}:${c.line}`);
}

// displayed preset catalog strings (translated at render time with t(variable))
{
  const ctx = { t: (s) => s };
  vm.createContext(ctx);
  const cat = vm.runInContext(read('web/presets.js') + '\n;({ PRESETS, PRESET_CATEGORIES, TAG_GROUPS })', ctx, { filename: 'web/presets.js' });
  const shown = (s, where) => { if (typeof s === 'string' && s && CJK.test(s)) need(s, where); };
  for (const [id, label] of cat.PRESET_CATEGORIES) shown(label, `web/presets.js PRESET_CATEGORIES ${id}`);
  for (const p of cat.PRESETS) {
    shown(p.name, `web/presets.js preset ${p.id} name`);
    shown(p.note, `web/presets.js preset ${p.id} note`);
    if (p.ua) shown(p.ua.note, `web/presets.js preset ${p.id} ua.note`);
  }
  for (const g of cat.TAG_GROUPS) {
    shown(g.label, `web/presets.js TAG_GROUPS ${g.id}`);
    for (const [id, label, desc] of g.items) {
      shown(label, `web/presets.js tag ${id} label`);
      shown(desc, `web/presets.js tag ${id} desc`);
    }
  }
}

if (missing.size) {
  failures.push(`${missing.size} string(s) have no English entry in I18N_EN (web/i18n.js):`);
  for (const [key, where] of missing) failures.push(`  ${where}: ${JSON.stringify(key)}`);
}

// English values must not contain Chinese text, and placeholders must match
for (const [key, val] of Object.entries(dict)) {
  if (typeof val !== 'string') { failures.push(`I18N_EN[${JSON.stringify(key)}] is not a string`); continue; }
  if (CJK.test(val)) failures.push(`I18N_EN[${JSON.stringify(key)}] English text contains Chinese characters: ${JSON.stringify(val)}`);
  const ph = (s) => [...s.matchAll(/\{(\w+)\}/g)].map((m) => m[1]).sort().join(',');
  if (ph(key) !== ph(val)) failures.push(`I18N_EN[${JSON.stringify(key)}] placeholders differ: {${ph(key)}} vs {${ph(val)}}`);
  const tags = (s) => (s.match(/<\/?[a-z][^>]*>/g) || []).sort().join('');
  if (tags(key) !== tags(val)) failures.push(`I18N_EN[${JSON.stringify(key)}] HTML markup differs from the source`);
}

// Chinese in web/app.js / web/account.js outside t() arguments and comments
for (const file of ['web/app.js', 'web/account.js']) {
  const src = read(file);
  const { calls, comments, tokens, lineOf } = tCalls(src, file);
  const chars = src.split('');
  const blank = (s, e) => { for (let k = s; k < e; k++) if (chars[k] !== '\n') chars[k] = ' '; };
  for (const [s, e] of comments) blank(s, e);
  for (const c of calls) blank(c.start, c.end);
  // regex literals are matching rules, not displayed text
  for (const tk of tokens) if (tk.type === 'regex') blank(tk.start, tk.end);
  const masked = chars.join('');
  for (const m of masked.matchAll(CJK_RUN)) {
    if (ALLOWED_RUNS.has(m[0])) continue;
    const line = lineOf(m.index);
    failures.push(`${file}:${line}: Chinese text outside t(): ${JSON.stringify(m[0])}  | ${src.split('\n')[line - 1].trim().slice(0, 120)}`);
  }
}

const unused = Object.keys(dict).filter((k) => !used.has(k));
if (unused.length) {
  console.warn(`warning: ${unused.length} unused I18N_EN key(s):`);
  for (const k of unused) console.warn('  ' + JSON.stringify(k));
}

if (failures.length) {
  console.error('i18n check failed:');
  for (const f of failures) console.error(f);
  process.exit(1);
}
console.log(`i18n check passed: ${Object.keys(dict).length} dictionary entries, ${used.size} strings in use.`);
