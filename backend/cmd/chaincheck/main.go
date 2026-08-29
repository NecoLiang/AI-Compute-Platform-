package main

// 文昌链联调自检工具 (T-057)。读取与主服务相同的配置(config.yaml + 环境变量):
//
//	go run ./cmd/chaincheck            连通性 + 链账户状态自检(只读)
//	go run ./cmd/chaincheck -attest    额外发送一条测试存证并回查比对(会消耗少量 gas)
//
// 典型用途: BSN 门户绑定链账户/充能量值之后, 跑一次 -attest 确认全链路打通。

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"tokenfactory/internal/blockchain"
	"tokenfactory/pkg/config"
)

func main() {
	attest := flag.Bool("attest", false, "发送一条测试存证并回查比对")
	cfgPath := flag.String("config", "config.yaml", "配置文件路径")
	flag.Parse()

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fail("加载配置失败: %v", err)
	}
	client, err := blockchain.NewBSNClient(blockchain.BSNConfig{
		GatewayURL:  cfg.Blockchain.GatewayURL,
		ProjectID:   cfg.Blockchain.ProjectID,
		ProjectKey:  cfg.Blockchain.ProjectKey,
		ChainID:     cfg.Blockchain.ChainID,
		AccountKey:  cfg.Blockchain.AccountKey,
		Denom:       cfg.Blockchain.Denom,
		GasLimit:    cfg.Blockchain.GasLimit,
		GasPrice:    cfg.Blockchain.GasPrice,
		ExplorerURL: cfg.Blockchain.ExplorerURL,
	})
	if err != nil {
		fail("装配客户端失败: %v", err)
	}
	if !client.Configured() {
		fail("未配置: 需 blockchain.gateway_url + project_id + account_key (BSN 门户「项目管理→下载接入参数」+「链账户」)")
	}
	fmt.Println("链账户地址:", client.Address())
	fmt.Println("压缩公钥  :", client.PubKeyHex())

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if !*attest {
		status, err := client.CheckAccount(ctx)
		if err != nil {
			fmt.Println("自检结果:", err)
			fmt.Println("(如提示链账户不存在: 到 BSN 门户「链账户」绑定上述地址并分配能量值)")
			os.Exit(1)
		}
		fmt.Println("链账户状态:", status)
		fmt.Println("✅ 网关连通、链账户已在链上, 可用 -attest 做一次真实存证联调")
		return
	}

	payload := map[string]string{"probe": "wanxiang-chaincheck", "at": time.Now().UTC().Format(time.RFC3339)}
	hash := blockchain.ComputeHash(payload)
	fmt.Println("测试存证 hash:", hash)

	txID, err := client.UploadHash(ctx, hash)
	if err != nil {
		fail("上链失败: %v", err)
	}
	fmt.Println("已上链, TX:", txID)
	if u := client.TxURL(txID); u != "" {
		fmt.Println("浏览器:", u)
	}

	ok, _, err := client.VerifyHash(ctx, txID, hash)
	if err != nil {
		fail("回查失败: %v", err)
	}
	if !ok {
		fail("回查比对不一致: 链上 digest 与本地 hash 不符")
	}
	fmt.Println("✅ 回查比对一致, 文昌链存证链路联调通过")
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
