/**
 * 进程内绑定（TypeScript）：与 JS 版同一个 N-API 插件，只是加了类型。
 *
 * Node 没有内建 FFI，所以这里必须走原生插件 —— 用 P/Invoke/ctypes 那类
 * "标准库直连"的路线在 Node 上不存在。
 */

import { createRequire } from 'node:module';
import { existsSync } from 'node:fs';
import { dirname, join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

const require = createRequire(import.meta.url);
const here = dirname(fileURLToPath(import.meta.url));

/** 原生层错误。code：2 = 输入/文件问题，3 = 执行失败。 */
export class NativeError extends Error {
  readonly code: number;
  constructor(message: string, code = 0) {
    super(message);
    this.name = 'NativeError';
    this.code = code;
  }
}

interface Addon {
  load(libraryPath: string): boolean;
  runFile(unitPath: string, inputJson: string): string;
}

function libraryName(): string {
  if (process.platform === 'darwin') return 'libhermesrt.dylib';
  if (process.platform === 'win32') return 'hermesrt.dll';
  return 'libhermesrt.so';
}

function candidates(name: string): string[] {
  const out: string[] = [];
  const home = process.env['HERMES_HOME'];
  if (home) out.push(join(home, 'lib', name));
  out.push(resolve(here, '..', '..', 'build', name));
  out.push(resolve(here, '..', '..', 'lib', name));
  out.push(resolve(here, '..', '..', '..', 'build', name));
  return out;
}

function firstExisting(list: string[]): string {
  for (const p of list) if (existsSync(p)) return p;
  return '';
}

/** 一次加载、多次执行的进程内运行时。 */
export class NativeRuntime {
  private readonly unit: string;
  private readonly addon: Addon;

  constructor(unit: string) {
    this.unit = unit;
    const addonPath = firstExisting(candidates('hermes.node'));
    if (!addonPath) {
      throw new NativeError(
        '找不到 hermes.node（Node 原生插件）。先跑 scripts/build_node_addon.sh。已尝试：' +
          candidates('hermes.node').join(', ')
      );
    }
    const libPath = firstExisting(candidates(libraryName()));
    if (!libPath) {
      throw new NativeError(
        '找不到 ' + libraryName() + '。发布包应自带 lib/；开发时先跑 scripts/build.sh。已尝试：' +
          candidates(libraryName()).join(', ')
      );
    }
    this.addon = require(addonPath) as Addon;
    this.addon.load(libPath);
  }

  /** 执行构造时给的那个 unit。 */
  run(input?: unknown): unknown {
    return this.runOn(this.unit, input);
  }

  /** 执行指定 unit。 */
  runOn(unit: string, input?: unknown): unknown {
    const payload = input === undefined || input === null ? '' : JSON.stringify(input);
    const out = this.addon.runFile(unit, payload);
    try {
      return JSON.parse(out) as unknown;
    } catch {
      return out;
    }
  }
}
