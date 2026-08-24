// Package helper 提供服务端通用的助手函数：
// 统一 JSON 响应封装（code + msg + data）、业务错误转响应等。
package helper

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/helloxz/ztag/internal/model"
)

// 统一响应的具体拼装（对外契约与 model.Response 一致）：
//   - code = 200  成功，msg 为空
//   - code = -1000 失败，msg 携带英文错误描述
//   - data 类型不固定，随场景变化；失败时为 null
type response struct {
	Code int         `json:"code"`
	Msg  string      `json:"msg"`
	Data interface{} `json:"data"`
}

// OK 输出成功响应。
func OK(c *gin.Context, data interface{}) {
	c.JSON(http.StatusOK, response{
		Code: model.CodeSuccess,
		Msg:  "",
		Data: data,
	})
}

// Fail 输出失败响应（失败码统一为 -1000）。
func Fail(c *gin.Context, msg string) {
	c.JSON(http.StatusOK, response{
		Code: model.CodeFailed,
		Msg:  msg,
		Data: nil,
	})
}

// FailWithError 将 error 转换为统一失败响应：
//   - *model.BizError 使用其自带描述；
//   - 其余错误统一返回 "internal server error"（避免把内部细节泄露给调用方）。
func FailWithError(c *gin.Context, err error) {
	var bizErr *model.BizError
	if AsBizError(err, &bizErr) {
		Fail(c, bizErr.Message)
		return
	}
	Fail(c, "internal server error")
}

// AsBizError 提取 error 链中的 *model.BizError（封装 errors.As，后续如需
// 扩展其他错误类型可在此统一收口）。
func AsBizError(err error, target **model.BizError) bool {
	if err == nil {
		return false
	}
	for current := err; current != nil; {
		if be, ok := current.(*model.BizError); ok {
			*target = be
			return true
		}
		// 解包一层（支持 fmt.Errorf("...: %w", err) 包装链）
		type unwrapper interface{ Unwrap() error }
		u, ok := current.(unwrapper)
		if !ok {
			return false
		}
		current = u.Unwrap()
	}
	return false
}
