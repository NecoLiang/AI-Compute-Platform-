package sms

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	openapiutil "github.com/alibabacloud-go/darabonba-openapi/v2/utils"
	dysmsapi "github.com/alibabacloud-go/dysmsapi-20170525/v5/client"
	"github.com/alibabacloud-go/tea/dara"
	"github.com/aliyun/credentials-go/credentials"
)

const defaultEndpoint = "dysmsapi.aliyuncs.com"

type AliyunSender struct {
	signName             string
	loginTemplateCode    string
	registerTemplateCode string
	send                 func(context.Context, *dysmsapi.SendSmsRequest, *dara.RuntimeOptions) (*dysmsapi.SendSmsResponse, error)
}

func NewAliyunSender(signName, loginTemplateCode, registerTemplateCode, endpoint string) (*AliyunSender, error) {
	if signName == "" || loginTemplateCode == "" || registerTemplateCode == "" {
		return nil, errors.New("短信签名、登录模板和注册模板不能为空")
	}
	if endpoint == "" {
		endpoint = defaultEndpoint
	}

	credential, err := credentials.NewCredential(nil)
	if err != nil {
		return nil, fmt.Errorf("create Alibaba Cloud credential provider: %w", err)
	}
	client, err := dysmsapi.NewClient(&openapiutil.Config{
		Credential:     credential,
		Endpoint:       dara.String(endpoint),
		Protocol:       dara.String("HTTPS"),
		ConnectTimeout: dara.Int(2000),
		ReadTimeout:    dara.Int(3000),
		RetryOptions:   &dara.RetryOptions{Retryable: false, MaxAttempts: 1},
	})
	if err != nil {
		return nil, fmt.Errorf("create Alibaba Cloud SMS client: %w", err)
	}

	return &AliyunSender{
		signName:             signName,
		loginTemplateCode:    loginTemplateCode,
		registerTemplateCode: registerTemplateCode,
		send:                 client.SendSmsWithContext,
	}, nil
}

func (s *AliyunSender) SendCode(ctx context.Context, phone, code, purpose string) error {
	templateCode := s.loginTemplateCode
	if purpose == "register" {
		templateCode = s.registerTemplateCode
	} else if purpose != "login" {
		return fmt.Errorf("unsupported SMS purpose: %s", purpose)
	}
	templateParam, err := json.Marshal(map[string]string{"code": code})
	if err != nil {
		return err
	}
	request := &dysmsapi.SendSmsRequest{
		PhoneNumbers:  dara.String(phone),
		SignName:      dara.String(s.signName),
		TemplateCode:  dara.String(templateCode),
		TemplateParam: dara.String(string(templateParam)),
	}
	runtime := &dara.RuntimeOptions{
		Autoretry:      dara.Bool(false),
		MaxAttempts:    dara.Int(1),
		ConnectTimeout: dara.Int(2000),
		ReadTimeout:    dara.Int(3000),
	}
	response, err := s.send(ctx, request, runtime)
	if err != nil {
		return fmt.Errorf("SendSms request failed: %w", err)
	}
	if response == nil || response.Body == nil || dara.StringValue(response.Body.Code) != "OK" {
		if response == nil || response.Body == nil {
			return errors.New("SendSms returned an empty response")
		}
		return fmt.Errorf("SendSms rejected: code=%s request_id=%s", dara.StringValue(response.Body.Code), dara.StringValue(response.Body.RequestId))
	}
	return nil
}
