package release

import "fmt"

// plannedMaterial 是数量核对规则对配方中一种物料的计算结果：
// 物料编号及其在当前计划份数下的应投量（千分之一克）。
type plannedMaterial struct {
	materialNo    string
	requiredGrams gramsMilli
}

// planRequirements 是批次“应投量核对”数量规则的唯一实现：给定批次采用
// 的配方版本 r 与最终采用的计划份数 portions，按配方中的物料顺序逐项计算
// 每份克数 × 计划份数，返回每种物料的应投量。
//
// 规则对创建批次、调整草稿、读取已保存批次和构造批次查询结果完全一致：
//   - 份数必须为正整数。份数的输入语义由各入口自行处理——创建要求正整数，
//     调整草稿时传 0 表示沿用当前份数（走到这里的必是最终正份数），读取
//     已保存批次时份数非正属于数据损坏；本函数只接受已确定的最终份数；
//   - 结果必须能精确表示到千分之一克：每份克数本身已是千分之一克的整数倍，
//     整数相乘不引入更小单位，不舍入、不截断；
//   - 每种物料分别以 maxGramsMilli（9223372036854775.807 克）为上限：
//     只乘该物料自己的每份克数与份数，不与同批次其他物料的应投量相加，
//     恰好等于上限合法，超过即返回错误。错误信息指出相关物料、配方编号、
//     版本与份数；调用方再按各自场景包装错误分类（提交时为
//     ErrInvalidInput，读取已保存批次时为 ErrCorruptData 并补充批次编号）。
//
// 不能改用同编号配方的其他版本：物料列表与每份克数一律取调用方给定的 r，
// 即批次实际绑定（或本次提交最终选定）的版本。
func planRequirements(r *recipeRecord, portions int) ([]plannedMaterial, error) {
	out := make([]plannedMaterial, 0, len(r.Materials))
	for _, m := range r.Materials {
		required, err := multiplyPortions(m.GramsMilli, portions)
		if err != nil {
			return nil, fmt.Errorf("物料 %q 按配方 %q 版本 %q 以 %d 份计算的应投量不合法: %v",
				m.MaterialNo, r.RecipeNo, r.Version, portions, err)
		}
		out = append(out, plannedMaterial{materialNo: m.MaterialNo, requiredGrams: required})
	}
	return out, nil
}
