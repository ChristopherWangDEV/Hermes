// 进程内绑定：用真插件 + 真动态库跑一个真 workflow（不经过网络）。
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { execFileSync } from 'node:child_process';
import { mkdtempSync, writeFileSync, existsSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join, resolve } from 'node:path';
import { NativeRuntime } from '../native.js';

const root = resolve(import.meta.dirname, '..', '..', '..');
const hermes = join(root, 'build', 'hermes');

test('进程内执行 workflow', { skip: !existsSync(hermes) && '没有 build/hermes' }, () => {
  const dir = mkdtempSync(join(tmpdir(), 'hermes-js-'));
  const src = join(dir, 'echo.hermes');
  writeFileSync(src,
    'export struct In { string text; }\n' +
    'export workflow echo(input: In) entry -> string { return "echo: {input.text}"; }\n');
  const hbc = join(dir, 'echo.hbc');
  execFileSync(hermes, ['build', src, '-o', hbc]);

  const rt = new NativeRuntime(hbc);
  assert.equal(rt.run({ text: 'hi' }), 'echo: hi');

  assert.throws(() => rt.runOn(join(dir, 'nope.hbc'), {}), (e) => e.name === 'Error');
});
