// support.h —— 对外接口。分发到没有源码的环境时，对方只需要这个文件 + support_bot.hbc。
//
// 这里**没有** #link：链接输入是 root unit 的事，不由头文件强加给消费者
// （否则每个 include 者都会继承它没选择的链接依赖——这正是 #link 要消灭的 C 式耦合）。

#include <std/net.h>            // tool.call / HttpError / NetworkError
#include <std/model.h>          // model.generate / ModelError / ContractError
#include "models/intent.h"
#include "models/order.h"

// 声明（无地址）。地址在 support.hermes 里定义，走环境注入。
export backend homonoia : openai;

// input 的类型必须 export —— 它同时是本 workflow 的外部请求契约，
// hermes serve 的 API schema 直接由它生成（LANGUAGE.md §4.6）。
export struct SupportInput {
    string text;
    string user_id;
}

// entry：本 unit 唯一的默认入口。因此 `hermes run support_bot.hbc` 可省略 --workflow。
// 返回类型在 .hermes 里由各 return 推断，.h 里必须写出（头文件没有体可推断）；
// 链接期校验两者一致（E_LINK_SIG_MISMATCH），编译期校验声明与推断一致（E3014）。
export workflow support_bot(input: SupportInput, backend = homonoia) entry -> string;
