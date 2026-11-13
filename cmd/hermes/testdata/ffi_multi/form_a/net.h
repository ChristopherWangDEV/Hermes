export struct NetResp { int status; string body; }
export extern workflow net_get(url: string) -> NetResp;
