// 进程内绑定：通过 N-API 插件调用 libhermesrt。
//
// 为什么需要插件：Node 没有内建 FFI（Python 有 ctypes、Java 有 FFM、C# 有
// P/Invoke，Node 没有）。N-API 是官方提供的零依赖路径 —— 我们只写一层薄绑定，
// 头文件由 Node 自带，不用 node-gyp / node-addon-api。
//
// 加载不到插件时**明确报错**，不静默退回到 HTTP：两者语义不同（一个进程内、
// 一个要起服务），悄悄换掉会让用户以为自己在跑本地代码。

import { createRequire } from 'node:module';
import { existsSync } from 'node:fs';
import { dirname, join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

const require = createRequire(import.meta.url);
const here = dirname(fileURLToPath(import.meta.url));

/** 原生层错误。code：2 = 输入/文件问题，3 = 执行失败。 */
export class NativeError extends Error {
  constructor(message, code = 0) {
    super(message);
    this.name = 'NativeError';
    this.code = code;
  }
}

function libraryName() {
  if (process.platform === 'darwin') return 'libhermesrt.dylib';
  if (process.platform === 'win32') return 'hermesrt.dll';
  return 'libhermesrt.so';
}

/** 按 HERMES_HOME → 仓库 build/ → 包旁 lib/ 的顺序找插件与动态库。 */
function candidates(name) {
  const out = [];
  if (process.env.HERMES_HOME) out.push(join(process.env.HERMES_HOME, 'lib', name));
  out.push(resolve(here, '..', '..', 'build', name));
  out.push(resolve(here, '..', '..', 'lib', name));
  out.push(resolve(here, '..', '..', '..', 'build', name));
  return out;
}

function firstExisting(list) {
  for (const p of list) if (existsSync(p)) return p;
  return '';
}

/**
 * 进程内运行时。
 *
 *   const rt = new NativeRuntime('app.hbc');
 *   const out = rt.run({ title: '打印机坏了' });
 */
export class NativeRuntime {
  constructor(unit) {
    this.unit = unit;
    this.addonPath = firstExisting(candidates('hermes.node'));
    if (!this.addonPath) {
      throw new NativeError(
        '找不到 hermes.node（Node 原生插件）。先跑 scripts/build_node_addon.sh。' +
          '已尝试：' + candidates('hermes.node').join(', ')
      );
    }
    this.libPath = firstExisting(candidates(libraryName()));
    if (!this.libPath) {
      throw new NativeError(
        '找不到 ' + libraryName() + '。发布包应自带 lib/；开发时先跑 scripts/build.sh。' +
          '已尝试：' + candidates(libraryName()).join(', ')
      );
    }
    // 插件是 .node，必须用 require 加载（ESM 里通过 createRequire）。
    this.addon = require(this.addonPath);
    this.addon.load(this.libPath);
  }

  /** 执行构造时给的那个 unit。 */
  run(input) {
    return this.runOn(this.unit, input);
  }

  /** 执行指定 unit。input 为对象时序列化成 JSON。 */
  runOn(unit, input) {
    const payload = input === undefined || input === null ? '' : JSON.stringify(input);
    const out = this.addon.runFile(unit, payload);
    try {
      return JSON.parse(out);
    } catch {
      return out; // 纯字符串结果原样返回
    }
  }
}
