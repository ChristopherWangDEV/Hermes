// models/intent.h —— 接口文件。相对 include 先查本文件所在目录（LANGUAGE.md §9.2）

#include "order.h"

export struct Intent {
    string action;      // "refund" / "logistics" / 其他（string 取值集合不受约束，故 match 必须有 default）
    OrderInfo order;
}
