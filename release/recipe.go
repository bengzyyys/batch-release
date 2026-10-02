package release

import (
	"encoding/json"
	"fmt"
)

const opRegisterRecipe = "registerRecipe"

type registerRecipePayload struct {
	RecipeNo  string
	Version   string
	Name      string
	Materials []MaterialInput
}

// RegisterRecipe 登记一个配方版本。
// 配方由配方编号与版本号共同识别；每个版本包含名称和至少一种物料，
// 物料记录编号和生产一份所需的克数（正数、最多三位小数）。
// 同一版本内物料编号不可重复；已登记的版本内容不可覆盖。
// 成功后返回配方视图；reqNo 用于幂等，重复提交相同内容返回同一结果。
func (s *Store) RegisterRecipe(reqNo, recipeNo, version, name string, materials []MaterialInput) (*RecipeView, error) {
	payload := registerRecipePayload{RecipeNo: recipeNo, Version: version, Name: name, Materials: materials}
	var out RecipeView
	err := s.write(reqNo, opRegisterRecipe, payload, func(st *persistedState) (json.RawMessage, error) {
		if recipeNo == "" {
			return nil, fmt.Errorf("%w: 配方编号不能为空", ErrInvalidInput)
		}
		if version == "" {
			return nil, fmt.Errorf("%w: 版本号不能为空", ErrInvalidInput)
		}
		if name == "" {
			return nil, fmt.Errorf("%w: 配方名称不能为空", ErrInvalidInput)
		}
		if len(materials) == 0 {
			return nil, fmt.Errorf("%w: 配方版本至少包含一种物料", ErrInvalidInput)
		}
		if findRecipe(st, recipeNo, version) != nil {
			return nil, fmt.Errorf("%w: 配方 %q 版本 %q 已存在", ErrRecipeExists, recipeNo, version)
		}

		rec := &recipeRecord{RecipeNo: recipeNo, Version: version, Name: name}
		seen := make(map[string]bool, len(materials))
		for _, m := range materials {
			if m.MaterialNo == "" {
				return nil, fmt.Errorf("%w: 物料编号不能为空", ErrInvalidInput)
			}
			if seen[m.MaterialNo] {
				return nil, fmt.Errorf("%w: 配方版本内物料编号 %q 重复", ErrInvalidInput, m.MaterialNo)
			}
			seen[m.MaterialNo] = true
			milli, err := parseGrams(m.Grams)
			if err != nil {
				return nil, fmt.Errorf("%w: 物料 %q 的克数: %v", ErrInvalidInput, m.MaterialNo, err)
			}
			rec.Materials = append(rec.Materials, materialRecord{MaterialNo: m.MaterialNo, GramsMilli: milli})
		}

		view := buildRecipeView(rec)
		raw, err := json.Marshal(view)
		if err != nil {
			return nil, err
		}
		st.Recipes = append(st.Recipes, rec)
		return raw, nil
	}, &out)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// GetRecipe 查询指定配方版本；不存在时返回 ErrNotFound。
func (s *Store) GetRecipe(recipeNo, version string) (*RecipeView, error) {
	var out *RecipeView
	err := s.read(func(st *persistedState) error {
		r := findRecipe(st, recipeNo, version)
		if r == nil {
			return fmt.Errorf("%w: 配方 %q 版本 %q", ErrNotFound, recipeNo, version)
		}
		out = buildRecipeView(r)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
