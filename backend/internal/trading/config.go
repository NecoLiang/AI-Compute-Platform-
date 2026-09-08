package trading

import (
	"errors"
	"fmt"
	"strconv"

	"github.com/jmoiron/sqlx"
)

var ErrDisabled = errors.New("交易暂时关闭，请稍后再试")

type Config struct {
	TradingEnabled bool `json:"trading_enabled"`
	FeeRate        int  `json:"fee_rate"`
}

const configQuery = "SELECT config_key, config_value FROM system_config WHERE config_key IN ('trading_enabled','fee_rate')"

func ReadConfig(db sqlx.Queryer) (Config, error) {
	return readConfig(db, configQuery)
}

// Hold the policy until the order commits; an admin update cannot overtake it.
func LockConfig(tx *sqlx.Tx) (Config, error) {
	return readConfig(tx, configQuery+" FOR SHARE")
}

func readConfig(db sqlx.Queryer, query string) (Config, error) {
	var rows []struct {
		Key   string `db:"config_key"`
		Value string `db:"config_value"`
	}
	if err := sqlx.Select(db, &rows, query); err != nil {
		return Config{}, err
	}
	// Missing or corrupt policy must not silently reopen trading or change fees.
	if len(rows) != 2 {
		return Config{}, fmt.Errorf("trading config is incomplete")
	}
	var config Config
	for _, row := range rows {
		switch row.Key {
		case "trading_enabled":
			if row.Value != "true" && row.Value != "false" {
				return Config{}, fmt.Errorf("invalid trading_enabled")
			}
			config.TradingEnabled = row.Value == "true"
		case "fee_rate":
			rate, err := strconv.Atoi(row.Value)
			if err != nil || rate < 0 || rate > 10000 {
				return Config{}, fmt.Errorf("invalid fee_rate")
			}
			config.FeeRate = rate
		}
	}
	return config, nil
}
