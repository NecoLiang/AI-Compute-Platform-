package sms

import (
	"context"
	"testing"

	dysmsapi "github.com/alibabacloud-go/dysmsapi-20170525/v5/client"
	"github.com/alibabacloud-go/tea/dara"
)

func TestAliyunSenderChoosesPurposeTemplateWithoutRetry(t *testing.T) {
	sender := &AliyunSender{
		signName:             "万象硅芯科技",
		loginTemplateCode:    "SMS_LOGIN",
		registerTemplateCode: "SMS_REGISTER",
	}
	wantTemplate := "SMS_LOGIN"
	sender.send = func(_ context.Context, request *dysmsapi.SendSmsRequest, runtime *dara.RuntimeOptions) (*dysmsapi.SendSmsResponse, error) {
		if got := dara.StringValue(request.PhoneNumbers); got != "13800138000" {
			t.Fatalf("phone = %q", got)
		}
		if got := dara.StringValue(request.SignName); got != sender.signName {
			t.Fatalf("sign = %q", got)
		}
		if got := dara.StringValue(request.TemplateCode); got != wantTemplate {
			t.Fatalf("template = %q", got)
		}
		if got := dara.StringValue(request.TemplateParam); got != `{"code":"123456"}` {
			t.Fatalf("template param = %q", got)
		}
		if dara.BoolValue(runtime.Autoretry) || dara.IntValue(runtime.MaxAttempts) != 1 {
			t.Fatalf("unexpected retry options: %+v", runtime)
		}
		return &dysmsapi.SendSmsResponse{Body: &dysmsapi.SendSmsResponseBody{Code: dara.String("OK")}}, nil
	}

	if err := sender.SendCode(context.Background(), "13800138000", "123456", "login"); err != nil {
		t.Fatal(err)
	}
	wantTemplate = "SMS_REGISTER"
	if err := sender.SendCode(context.Background(), "13800138000", "123456", "register"); err != nil {
		t.Fatal(err)
	}
}
