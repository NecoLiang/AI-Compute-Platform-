package compute

import "fmt"

// CardsPerUnit converts a product's trading unit using its uniform GPU specification.
func CardsPerUnit(productType string, cardCount, machineCount int) (int, error) {
	switch productType {
	case ProductTypeCardRental:
		return 1, nil
	case ProductTypeOutright, ProductTypeCenter:
		if cardCount <= 0 || machineCount <= 0 || cardCount%machineCount != 0 {
			return 0, fmt.Errorf("商品规格无法确定每台卡数, 请供应方核对总卡数与台数")
		}
		return cardCount / machineCount, nil
	default:
		return 0, fmt.Errorf("该商品不按 GPU 容量交付")
	}
}
