// models/order.h —— 接口文件（只有声明，没有实现；因此也不需要 include guard）
// include-once 是语义保证（LANGUAGE.md §9.3），不是靠预处理宏。

export struct OrderInfo {
    string order_id;
    int status;         // 业务状态码。注意：这不是 HTTP 状态码（见 ../README.md §4）
    cost amount;        // D-010.1：金额禁止 float，D-018：类型名 cost
}
