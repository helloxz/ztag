// Package model 定义领域模型：请求体、响应体与业务错误。
package model

import "errors"

// 统一响应错误码（对外契约）：
//   - 成功固定返回 200；
//   - 失败一律返回 -1000，具体原因通过 msg 字段描述。
//
// 不对外细分错误码，调用方只需判断 code 是否为 200。
const (
	CodeSuccess = 200   // 成功
	CodeFailed  = -1000 // 失败
)

// 预置的典型业务错误，供内部 errors.Is 判断与单元测试使用；
// 对外不区分错误码，统一映射为 code=-1000（由 helper 层转换）。
var (
	ErrInvalidParam     = errors.New("invalid request parameters")
	ErrImageFetchFailed = errors.New("failed to fetch image")
	ErrImageInvalid     = errors.New("invalid image format or size")
	ErrAIUnavailable    = errors.New("AI channel unavailable")
)

// BizError 业务错误：仅携带对外展示的错误描述（统一英文消息），
// 供 service 层向上抛出、handler 层统一转换为 JSON 响应。
type BizError struct {
	Message string // 错误描述（英文，对外展示）
}

// Error 实现 error 接口。
func (e *BizError) Error() string {
	return e.Message
}

// NewBizError 构造一个业务错误。
func NewBizError(msg string) *BizError {
	return &BizError{Message: msg}
}
