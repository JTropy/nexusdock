// Package buildinfo 承载构建期通过 go build -ldflags -X 注入的版本信息。
// 本地源码构建（go run / go test）不会注入，保持缺省值 dev/unknown/unknown，
// 用于区分开发构建与正式构建；/v1/system/status 直接读取这些值上报。
package buildinfo

var (
	// Version 由 Makefile 注入 git describe --tags --always --dirty 的结果。
	Version = "dev"
	// Revision 由 Makefile 注入 git rev-parse HEAD 的完整提交号。
	Revision = "unknown"
	// Source 预留镜像来源（如仓库地址），由容器构建参数注入，缺省 unknown。
	Source = "unknown"
)
