// Package webui 管理台静态资源（HTML / CSS / JS）。
//
// 默认把 web 目录内嵌进二进制，保证发布物只有一个可执行文件；
// 开发时可用 UseDir 切换为读磁盘目录，改完页面刷新即生效，无需重启服务。
package webui

import (
	"embed"
	"fmt"
	"io/fs"
	"os"
)

//go:embed web
var embedded embed.FS

// FS 当前生效的资源文件系统（根即 web 目录）。
// 默认是内嵌资源；调用 UseDir 后切换为磁盘目录。
var FS fs.FS = mustSubEmbedded()

// fromDisk 标记当前资源是否来自磁盘（开发模式）。
var fromDisk bool

// mustSubEmbedded 把内嵌资源挂到 web 目录下，保证两种来源的路径一致。
func mustSubEmbedded() fs.FS {
	sub, err := fs.Sub(embedded, "web")
	if err != nil {
		panic(fmt.Sprintf("webui: failed to sub embedded assets: %v", err))
	}
	return sub
}

// UseDir 切换为从磁盘目录读取资源（开发模式）。
// 目录不存在或不是目录时返回错误，由调用方决定是否回退内嵌资源。
func UseDir(dir string) error {
	info, err := os.Stat(dir)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a directory", dir)
	}
	FS = os.DirFS(dir)
	fromDisk = true
	return nil
}

// FromDisk 当前资源是否来自磁盘。
// handler 据此在开发模式下发 no-store，避免改完文件浏览器还拿缓存。
func FromDisk() bool {
	return fromDisk
}
