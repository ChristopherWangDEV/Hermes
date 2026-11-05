package header

import (
	"fmt"
	"strings"

	"hermes/internal/ir"
)

// GenCExe 生成一个**独立可执行文件**的 C 源：内嵌 IR 字节，调运行时入口。
//
// 为什么内嵌字节而不是运行时读文件：读文件意味着产物不再是"单文件"——
// 部署时要么多带一个 .ir.json，要么把路径写死，而路径写死就不可重现。
func GenCExe(doc *ir.Doc, irBytes []byte) ([]byte, error) {
	var b strings.Builder

	b.WriteString("/* 由 hermes compile 生成，**不要手改**。 */\n")
	b.WriteString("/* 内嵌 IR + 链接运行时静态库 => 不依赖已安装的 hermes。 */\n\n")
	b.WriteString("#include <stdio.h>\n#include <stdlib.h>\n#include <string.h>\n\n")
	b.WriteString("#include \"hermes_rt.h\"\n\n")

	// IR 字节：每行 16 个，写成十六进制转义。C89 兼容（数组初始化、无 C99 语法）。
	b.WriteString("static const unsigned char hermes_ir_bytes[] = {\n")
	for i := 0; i < len(irBytes); i += 16 {
		end := i + 16
		if end > len(irBytes) {
			end = len(irBytes)
		}
		b.WriteString("  ")
		for _, c := range irBytes[i:end] {
			fmt.Fprintf(&b, "0x%02x,", c)
		}
		b.WriteString("\n")
	}
	b.WriteString("  0x00\n};\n\n")

	fmt.Fprintf(&b, "#define HERMES_IR_LEN ((size_t)%d)\n\n", len(irBytes))

	b.WriteString(`int main(int argc, char** argv) {
    char* out = NULL;
    char* err = NULL;
    const char* input = NULL;
    int rc = 0;

    /* argv[1] 是**输入 JSON**（与 hermes run --input 同一个契约）。
     * 不给参数时 input 为 NULL —— workflow 若需要输入会明确报错，
     * 而不是拿一个空对象继续跑（那样错误会出现在很远的地方）。 */
    if (argc > 1) {
        input = argv[1];
    }
    rc = hermes_rt_run((char*)hermes_ir_bytes, HERMES_IR_LEN,
                       (char*)input, input ? strlen(input) : 0,
                       &out, &err);
    if (rc != 0) {
        fprintf(stderr, "hermes: %s\n", err ? err : "(no message)");
        free(err);
        return rc;
    }
    if (out) {
        printf("%s\n", out);
        free(out);
    }
    return 0;
}
`)
	return []byte(b.String()), nil
}
