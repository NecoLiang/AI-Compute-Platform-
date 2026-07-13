# 区块链存证 Blockchain API

**Base**: `http://localhost:8080/api/v1` | **Auth**: 验证接口公开，查询需登录

> ⚠️ **BSN-DDC 未接入**。存证事件可写入本地 DB，但链上存证需 BSN API Key 接入后生效。

---

## GET /blockchain/verify · 验证链上存证（公开）

```
curl "http://localhost:8080/api/v1/blockchain/verify?type=order&id=123"
```

| 参数 | 类型 | 必填 | 说明 |
|------|------|:--:|------|
| type | string | ✅ | order / delivery / credit_score |
| id | string | ✅ | 对应的业务 ID |

**已接入 BSN 后**
```json
{"code":0,"data":{
  "verified":true,
  "tx_id":"0x7a3b...c21d",
  "chain_timestamp":"2026-07-13T14:30:18Z",
  "verify_url":"https://bsnscan.com/tx/0x7a3b...c21d"
}}
```

**BSN 未接入 / 无存证记录**
```json
{"code":0,"data":{"verified":false}}
```

---

## GET /blockchain/attestations/:target_type/:target_id · 存证记录

```
curl http://localhost:8080/api/v1/blockchain/attestations/order/123 \
  -H "Authorization: Bearer <token>"
```
