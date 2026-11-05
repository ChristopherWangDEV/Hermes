// 契约放这里：类型、backend 声明、工具声明。
#include <std/model.h>
#include <std/net.h>

export struct Ticket {
    string text;
    string user_id;
}

export struct Intent {
    string action;
    string order_id;
}

export backend gateway : openai;

export tool lookup_order {
    description = "按订单号查订单状态";
    url = "http://127.0.0.1:8080/orders/{id}";
    method = "GET";
}

export workflow triage(input: Ticket, backend = gateway) entry -> string;
