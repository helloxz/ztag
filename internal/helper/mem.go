// Package helper 提供服务端通用的助手函数（含内存探测等容量决策辅助）。
package helper

import (
	"os"
	"strconv"
	"strings"
)

// 内存探测相关常量：超过该值视为"无内存限制"（cgroup v1 未限制时
// limit_in_bytes = 9223372036854771712，v2 为 "max" 或超大数值），
// 回退到宿主机物理内存或保守兜底。
const noMemoryLimitThreshold int64 = 1 << 40 // 1TB

// MemoryLimitBytes 返回进程可用的内存上限（字节），用于容量相关的动态决策（如并发数）：
//  1. 优先读 cgroup v2 的 memory.max（容器场景）；
//  2. 其次读 cgroup v1 的 memory.limit_in_bytes；
//  3. 检测到处于容器内但 cgroup 限制读不到（非标准挂载布局）→ 返回 0（保守兜底）；
//  4. 非容器（裸机）→ 回退宿主机物理内存 /proc/meminfo 的 MemTotal。
//
// 返回 0 时调用方应按最保守策略处理（见 service.concurrencyLimitByMemory）。
func MemoryLimitBytes() int64 {
	if v := readCgroupV2(); v > 0 {
		return v
	}
	if v := readCgroupV1(); v > 0 {
		return v
	}
	// 容器内但限制读不到：绝不能按宿主物理内存判定并发（会高估导致 OOM），保守返回 0
	if inContainer() {
		return 0
	}
	return readMemTotal()
}

// readCgroupV2 读取 cgroup v2 内存上限：/sys/fs/cgroup/memory.max。
// 内容为字节数或 "max"（无限制）；无限制/超阈值/读取失败返回 0。
func readCgroupV2() int64 {
	b, err := os.ReadFile("/sys/fs/cgroup/memory.max")
	if err != nil {
		return 0
	}
	s := strings.TrimSpace(string(b))
	if s == "" || s == "max" {
		return 0
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil || v <= 0 || v > noMemoryLimitThreshold {
		return 0
	}
	return v
}

// readCgroupV1 读取 cgroup v1 内存上限：/sys/fs/cgroup/memory/memory.limit_in_bytes。
// 未限制（值超过 noMemoryLimitThreshold）或读取失败返回 0。
func readCgroupV1() int64 {
	b, err := os.ReadFile("/sys/fs/cgroup/memory/memory.limit_in_bytes")
	if err != nil {
		return 0
	}
	v, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	if err != nil || v <= 0 || v > noMemoryLimitThreshold {
		return 0
	}
	return v
}

// inContainer 根据 /proc/self/cgroup 中的路径关键字判断是否运行在容器内。
// 标准 docker/k8s/containerd 运行时在容器内的 cgroup 路径含对应关键字；
// 非容器的 systemd 宿主路径（如 /user.slice/...）不含这些关键字。
func inContainer() bool {
	b, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return false
	}
	s := string(b)
	for _, kw := range []string{"docker", "kubepods", "containerd", "libpod"} {
		if strings.Contains(s, kw) {
			return true
		}
	}
	return false
}

// readMemTotal 读取宿主机物理内存总量（/proc/meminfo 的 MemTotal 行）。
func readMemTotal() int64 {
	b, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, "MemTotal:") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				if kb, err := strconv.ParseInt(fields[1], 10, 64); err == nil {
					return kb * 1024
				}
			}
			break
		}
	}
	return 0
}
