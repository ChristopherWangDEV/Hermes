// 只声明本 SDK 用到的那些 Node API。
//
// 为什么不装 @types/node：那是几百 KB 的类型包，而我们只用 5 个函数。
// 更实际的理由是依赖纪律：SDK 的**构建**依赖也会出现在使用者的 lock 文件里，
// 而这里手写 30 行声明就能避免。

declare module 'node:module' {
  export function createRequire(url: string | URL): (id: string) => unknown;
}

declare module 'node:fs' {
  export function existsSync(path: string): boolean;
}

declare module 'node:path' {
  export function dirname(p: string): string;
  export function join(...parts: string[]): string;
  export function resolve(...parts: string[]): string;
}

declare module 'node:url' {
  export function fileURLToPath(url: string | URL): string;
}

interface ImportMeta {
  url: string;
}

declare const process: {
  platform: string;
  env: Record<string, string | undefined>;
};

declare const require: (id: string) => unknown;
