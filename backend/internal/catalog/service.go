package catalog

import (
	"fmt"
	"strings"
)

type Service struct{ repo *Repository }

func NewService(repo *Repository) *Service { return &Service{repo: repo} }

var validOrigins = map[string]bool{"domestic": true, "international": true}
var validGrades = map[string]bool{"datacenter": true, "consumer": true}
var validStatus = map[string]bool{"enabled": true, "disabled": true}

// PublicList 前端下拉用: 只出 enabled, 且不暴露 spec_source(内部运营字段)。
func (s *Service) PublicList(f Filter) ([]GPUModel, error) {
	f.IncludeDisabled = false
	list, err := s.repo.List(f)
	if err != nil {
		return nil, err
	}
	for i := range list {
		list[i].SpecSource = nil
	}
	return list, nil
}

func (s *Service) AdminList(f Filter) ([]GPUModel, error) {
	f.IncludeDisabled = true
	return s.repo.List(f)
}

func validate(m *GPUModel) error {
	m.Vendor = strings.TrimSpace(m.Vendor)
	m.ModelName = strings.TrimSpace(m.ModelName)
	if m.Vendor == "" || m.ModelName == "" {
		return fmt.Errorf("vendor 与 model_name 必填")
	}
	if !validOrigins[m.Origin] {
		return fmt.Errorf("origin 须为 domestic/international")
	}
	if !validGrades[m.Grade] {
		return fmt.Errorf("grade 须为 datacenter/consumer")
	}
	if m.VRAMGB != nil && *m.VRAMGB <= 0 {
		return fmt.Errorf("vram_gb 须为正数")
	}
	if m.FP16TFLOPS != nil && *m.FP16TFLOPS <= 0 {
		return fmt.Errorf("fp16_tflops 须为正数")
	}
	return nil
}

func (s *Service) Create(m *GPUModel) error {
	if err := validate(m); err != nil {
		return err
	}
	if err := s.repo.Create(m); err != nil {
		if strings.Contains(err.Error(), "Duplicate") {
			return fmt.Errorf("型号 %s 已存在", m.ModelName)
		}
		return err
	}
	return nil
}

func (s *Service) Update(id int64, m *GPUModel) (*GPUModel, error) {
	if !validStatus[m.Status] {
		return nil, fmt.Errorf("status 须为 enabled/disabled")
	}
	if err := validate(m); err != nil {
		return nil, err
	}
	existing, err := s.repo.Get(id)
	if err != nil {
		return nil, err
	}
	if existing == nil {
		return nil, fmt.Errorf("型号不存在")
	}
	m.ID = id
	if err := s.repo.Update(m); err != nil {
		if strings.Contains(err.Error(), "Duplicate") {
			return nil, fmt.Errorf("型号 %s 已存在", m.ModelName)
		}
		return nil, err
	}
	return s.repo.Get(id)
}
