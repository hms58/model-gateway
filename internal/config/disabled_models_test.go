package config

import (
	"errors"
	"strings"
	"testing"
)

// disabledFixture 一个产品 + 一个组，组里用了 workbuddy/gpt-5。
func disabledFixture() Config {
	cfg := Default()
	cfg.Products = []Product{{ID: "workbuddy", Enabled: true, BaseURL: "http://127.0.0.1:7863"}}
	cfg.ModelGroups = []Group{{
		Name: "gpt-5", Enabled: true,
		Members: []Member{{Product: "workbuddy", Model: "gpt-5", Weight: 100, Enabled: true}},
	}}
	return cfg
}

// 停用与在用互斥：停用集里的 (产品, 模型) 不能出现在任何模型组里。
func TestValidateRejectsDisabledModelInUse(t *testing.T) {
	cfg := disabledFixture()
	cfg.Products[0].DisabledModels = []string{"gpt-5"}

	err := Validate(&cfg)
	if err == nil {
		t.Fatal("停用在用模型应被拒绝")
	}
	if !errors.Is(err, ErrInvalidGroup) {
		t.Fatalf("应归到 ErrInvalidGroup（面板映射 400），实际 %v", err)
	}
	for _, want := range []string{"gpt-5", "已被停用"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("错误信息应包含 %q：%v", want, err)
		}
	}
}

// 同名模型在不同产品下是独立开关：只停用 trae 的，不影响 workbuddy 在用的那个。
func TestValidateAllowsDisabledModelNotInUse(t *testing.T) {
	cfg := disabledFixture()
	cfg.Products = append(cfg.Products, Product{ID: "trae", Enabled: true, BaseURL: "http://127.0.0.1:7864"})
	cfg.Products[0].DisabledModels = []string{"gpt-5-mini"}
	cfg.Products[1].DisabledModels = []string{"gpt-5"}

	if err := Validate(&cfg); err != nil {
		t.Fatalf("未被引用的停用模型应允许：%v", err)
	}
}

func TestSetModelDisabledIsIdempotent(t *testing.T) {
	product := Product{ID: "workbuddy"}
	product.SetModelDisabled("global:glm-5.3", true)
	product.SetModelDisabled("global:glm-5.3", true)
	if len(product.DisabledModels) != 1 {
		t.Fatalf("重复停用不应重复写入：%+v", product.DisabledModels)
	}
	if !product.ModelDisabled("global:glm-5.3") {
		t.Fatal("应判定为已停用")
	}
	// 精确匹配：去掉域前缀的同名模型是另一个开关。
	if product.ModelDisabled("glm-5.3") {
		t.Fatal("停用状态必须精确匹配，不能被前缀不同的同名模型命中")
	}

	product.SetModelDisabled("global:glm-5.3", false)
	if product.DisabledModels != nil {
		t.Fatalf("取消到空时应置 nil（不留空数组）：%+v", product.DisabledModels)
	}
	product.SetModelDisabled("global:glm-5.3", false)
	if product.DisabledModels != nil {
		t.Fatalf("重复取消应保持 nil：%+v", product.DisabledModels)
	}
}

// Clone 必须深拷贝停用切片：否则面板改副本会写进正在运行的快照。
func TestCloneCopiesDisabledModels(t *testing.T) {
	cfg := disabledFixture()
	cfg.Products[0].DisabledModels = []string{"a"}

	clone := cfg.Clone()
	clone.Products[0].SetModelDisabled("b", true)
	if len(cfg.Products[0].DisabledModels) != 1 {
		t.Fatalf("改副本不应影响原配置：%+v", cfg.Products[0].DisabledModels)
	}
}
