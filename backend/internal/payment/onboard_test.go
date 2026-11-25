package payment

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// onboardServer 模拟易宝网关: 记录请求并返回带平台签名的 result。
func onboardServer(t *testing.T, result func(r *http.Request) string) (*YeepayClient, *[]*http.Request, *[][]byte) {
	t.Helper()
	_, merchantKey := testRSAKey(t)
	platform, _ := testRSAKey(t)
	var reqs []*http.Request
	var bodies [][]byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		reqs, bodies = append(reqs, r), append(bodies, body)
		payload := "{\"result\":" + result(r) + "}"
		w.Header().Set("x-yop-sign", rsaSign(t, platform, yopSignWhitespace.ReplaceAllString(payload, "")))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(payload))
	}))
	t.Cleanup(server.Close)
	client, err := NewYeepayClient(YeepayConfig{AppKey: "app_1", ParentMerchantNo: "10090000001", MerchantNo: "10090000001",
		PrivateKey: merchantKey, PlatformPublicKey: testPublicKey(t, platform), ServerRoot: server.URL,
		NotifyBaseURL: "https://www.omnisline.com/api/v1", ReturnBaseURL: "https://omnisline.com", PayChannels: "bank",
		NetInShareScene: "FZ_FJ001"})
	require.NoError(t, err)
	return client, &reqs, &bodies
}

func TestUploadQualificationSendsSignedMultipart(t *testing.T) {
	client, reqs, bodies := onboardServer(t, func(*http.Request) string {
		return `{"returnCode":"REG00000","returnMsg":"成功","merQualUrl":"https://staticres.yeepay.com/abc.png"}`
	})
	got, err := client.UploadQualification("qualification.png", []byte("\x89PNG-bytes"))
	require.NoError(t, err)
	assert.Equal(t, "https://staticres.yeepay.com/abc.png", got)
	r := (*reqs)[0]
	assert.Equal(t, "/yos/v1.0/sys/merchant/qual/upload", r.URL.Path)
	assert.True(t, strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data; boundary="))
	assert.True(t, strings.HasPrefix(r.Header.Get("Authorization"), "YOP-RSA2048-SHA256 yop-auth-v3/app_1/"))
	assert.Equal(t, sha256Hex(nil), r.Header.Get("x-yop-content-sha256"), "文件不参与签名: 摘要取空参数串")
	body := string((*bodies)[0])
	assert.Contains(t, body, `name="merQual"; filename="qualification.png"`)
	assert.Contains(t, body, "\x89PNG-bytes")
}

func TestUploadQualificationSurfacesYeepayRejection(t *testing.T) {
	client, _, _ := onboardServer(t, func(*http.Request) string {
		return `{"returnCode":"REG30901","returnMsg":"文件上传异常"}`
	})
	_, err := client.UploadQualification("qualification.png", []byte("x"))
	require.Error(t, err)
	assert.Equal(t, "REG30901", bizCode(err))
}

func TestRegisterShareMerchantRequestShape(t *testing.T) {
	client, reqs, bodies := onboardServer(t, func(r *http.Request) string {
		if r.Method == http.MethodGet {
			return `{"returnCode":"NIG00000","requestNo":"NI9-x","applicationStatus":"REVIEW_BACK","auditOpinion":"营业执照不清晰","merchantNo":"10090001234"}`
		}
		return `{"returnCode":"NIG00000","returnMsg":"请求成功","requestNo":"NI9-x","applicationNo":"TYSHRW1","applicationStatus":"REVIEWING","merchantNo":"10090001234"}`
	})
	res, err := client.RegisterShareMerchant(ShareMerchantApplication{
		RequestNo: "NI9-x", SignName: "北京测试科技有限公司", ShortName: "测试科技", LicenceNo: "91110000MA00000000", LicenceURL: "u-lic",
		OpenAccountLicenceURL: "u-open", LegalName: "张三", LegalLicenceNo: "11010119900101123X", LegalFrontURL: "u-front", LegalBackURL: "u-back",
		ContactName: "李四", ContactMobile: "13800000000", ContactEmail: "a@b.cn", Province: "110000", City: "110100", District: "110105",
		Address: "某某路 1 号", BankCardNo: "1234567890123", CnapsCode: "102100099996", ShareCertificate: "u-cert",
	})
	require.NoError(t, err)
	assert.Equal(t, "REVIEWING", res.ApplicationStatus)
	assert.Equal(t, "/rest/v2.0/mer/register/saas/merchant", (*reqs)[0].URL.Path)

	// 表单值按官方 SDK 双重编码: 解一次表单, 再 QueryUnescape 一次。
	form, err := url.ParseQuery(string((*bodies)[0]))
	require.NoError(t, err)
	field := func(k string) string {
		v, err := url.QueryUnescape(form.Get(k))
		require.NoError(t, err)
		return v
	}
	obj := func(k string) map[string]string {
		m := map[string]string{}
		require.NoError(t, json.Unmarshal([]byte(field(k)), &m), k)
		return m
	}
	assert.Equal(t, "SHARE_MERCHANT", field("businessRole"))
	assert.Equal(t, "10090000001", field("parentMerchantNo"), "上级为平台商编")
	assert.Equal(t, "https://www.omnisline.com/api/v1/payment/yeepay/notify/netin", field("notifyUrl"))
	assert.Equal(t, map[string]string{"signType": "ENTERPRISE", "licenceNo": "91110000MA00000000", "licenceUrl": "u-lic",
		"signName": "北京测试科技有限公司", "shortName": "测试科技", "openAccountLicenceUrl": "u-open"}, obj("merchantSubjectInfo"))
	assert.Equal(t, "ID_CARD", obj("merchantCorporationInfo")["legalLicenceType"])
	assert.Equal(t, map[string]string{"settlementDirection": "BANKCARD", "bankAccountType": "ENTERPRISE_ACCOUNT",
		"bankCardNo": "1234567890123", "cnapsCode": "102100099996"}, obj("settlementAccountInfo"))
	assert.Equal(t, map[string]string{"province": "110000", "city": "110100", "district": "110105", "address": "某某路 1 号"}, obj("businessAddressInfo"))
	assert.JSONEq(t, `[{"productCode":"D1","rateType":"SINGLE_FIXED","fixedRate":"0"}]`, field("productInfo"), "默认 D1、手续费 0")
	assert.Equal(t, map[string]string{"shareScene": "FZ_FJ001", "shareCertificate": "u-cert"}, obj("functionServiceQualificationInfo"))

	q, err := client.QueryNetIn("NI9-x")
	require.NoError(t, err)
	assert.Equal(t, "REVIEW_BACK", q.ApplicationStatus)
	assert.Equal(t, "营业执照不清晰", q.AuditOpinion)
	assert.Equal(t, "/rest/v2.0/mer/register/query", (*reqs)[1].URL.Path)
}

func TestRegisterShareMerchantSurfacesValidationError(t *testing.T) {
	client, _, _ := onboardServer(t, func(*http.Request) string {
		return `{"returnCode":"NIG00002","returnMsg":"经营省编码有误"}`
	})
	_, err := client.RegisterShareMerchant(ShareMerchantApplication{RequestNo: "NI9-x"})
	require.Error(t, err)
	assert.Equal(t, "NIG00002", bizCode(err))
	assert.Contains(t, err.Error(), "经营省编码有误")
}

func validOnboardApplication() OnboardApplication {
	return OnboardApplication{SignName: " 北京测试科技有限公司 ", ShortName: "测试科技", LicenceNo: "91110000ma00000000", LicenceURL: "u-lic",
		LegalName: "张三", LegalLicenceNo: "11010119900101123x", LegalFrontURL: "u-front", LegalBackURL: "u-back",
		ContactName: "李四", ContactMobile: "13800000000", ContactEmail: "a@b.cn", Province: "110000", City: "110100", District: "110105",
		Address: "某某路 1 号院", BankCardNo: "1234567890123", CnapsCode: "102100099996"}
}

func TestOnboardApplicationValidation(t *testing.T) {
	app := validOnboardApplication()
	require.NoError(t, app.normalize())
	assert.Equal(t, "北京测试科技有限公司", app.SignName)
	assert.Equal(t, "91110000MA00000000", app.LicenceNo)
	assert.Equal(t, "11010119900101123X", app.LegalLicenceNo)

	for name, tc := range map[string]struct {
		mutate func(*OnboardApplication)
		want   string
	}{
		"信用代码":    {func(a *OnboardApplication) { a.LicenceNo = "123" }, "统一社会信用代码"},
		"简称纯数字":   {func(a *OnboardApplication) { a.ShortName = "800" }, "简称"},
		"身份证":     {func(a *OnboardApplication) { a.LegalLicenceNo = "1101" }, "身份证"},
		"手机":      {func(a *OnboardApplication) { a.ContactMobile = "010-123" }, "手机号"},
		"邮箱":      {func(a *OnboardApplication) { a.ContactEmail = "nope" }, "邮箱"},
		"省市区":     {func(a *OnboardApplication) { a.District = "" }, "省市区"},
		"地址过短":    {func(a *OnboardApplication) { a.Address = "1号" }, "详细地址"},
		"账号":      {func(a *OnboardApplication) { a.BankCardNo = "abc" }, "结算账号"},
		"联行号":     {func(a *OnboardApplication) { a.CnapsCode = "1021" }, "联行号"},
		"缺营业执照":   {func(a *OnboardApplication) { a.LicenceURL = "" }, "营业执照"},
		"缺身份证照":   {func(a *OnboardApplication) { a.LegalBackURL = "" }, "身份证正反面"},
		"文件地址含空白": {func(a *OnboardApplication) { a.LicenceURL = "a b" }, "营业执照"},
	} {
		t.Run(name, func(t *testing.T) {
			a := validOnboardApplication()
			tc.mutate(&a)
			err := a.normalize()
			if assert.Error(t, err) {
				assert.Contains(t, err.Error(), tc.want)
			}
		})
	}
}

func TestUploadOnboardFileRejectsBadFilesBeforeCallingYeepay(t *testing.T) {
	svc := &Service{yeepay: &YeepayClient{}}
	_, err := svc.UploadOnboardFile("a.exe", []byte("MZ\x90\x00 not an image"))
	assert.ErrorContains(t, err, "仅支持")
	_, err = svc.UploadOnboardFile("a.png", nil)
	assert.ErrorContains(t, err, "6MB")
	_, err = svc.UploadOnboardFile("a.png", make([]byte, onboardMaxFileBytes+1))
	assert.ErrorContains(t, err, "6MB")
	// 合法图片但易宝未接入: 明确报未接入, 不假装成功。
	_, err = svc.UploadOnboardFile("a.png", []byte("\x89PNG\r\n\x1a\n0000"))
	assert.ErrorIs(t, err, ErrYeepayNotConfigured)
}

func TestOnboardViewStatuses(t *testing.T) {
	assert.Equal(t, "not_started", onboardView(nil).Status)
	for status, want := range map[string]string{"INIT": "not_started", "REVIEWING": "pending", "AUTHENTICITY_VERIFYING": "pending",
		"BUSINESS_OPENING": "pending", "REVIEW_BACK": "rejected", "COMPLETED": "completed"} {
		assert.Equal(t, want, onboardView(&Merchant{ApplicationStatus: status}).Status, status)
	}
}
