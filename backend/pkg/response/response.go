package response

import (
	"net/http"
	"github.com/gin-gonic/gin"
)

type Response struct {
	Code      int         `json:"code"`
	Message   string      `json:"message"`
	Data      interface{} `json:"data,omitempty"`
	RequestID string      `json:"request_id"`
}

type PageData struct {
	List     interface{} `json:"list"`
	Total    int64       `json:"total"`
	Page     int         `json:"page"`
	PageSize int         `json:"page_size"`
}

func Success(c *gin.Context, data interface{}) {
	c.JSON(http.StatusOK, Response{
		Code:      0,
		Message:   "success",
		Data:      data,
		RequestID: c.GetString("request_id"),
	})
}

func SuccessPage(c *gin.Context, list interface{}, total int64, page, pageSize int) {
	Success(c, PageData{List: list, Total: total, Page: page, PageSize: pageSize})
}

func Error(c *gin.Context, code int, msg string) {
	c.JSON(http.StatusOK, Response{
		Code:      code,
		Message:   msg,
		RequestID: c.GetString("request_id"),
	})
}

func ErrorWithStatus(c *gin.Context, httpStatus, code int, msg string) {
	c.JSON(httpStatus, Response{
		Code:      code,
		Message:   msg,
		RequestID: c.GetString("request_id"),
	})
}
