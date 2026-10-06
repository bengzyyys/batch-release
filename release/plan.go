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

// validateUnfedRequirements 是“无投料批次结果”逐物料数量核对的唯一实现，
// 创建批次、草稿调整与开始执行三类已保存成功请求在读取台账时共用这里，不再
// 各自维护一份相同的核对。mats 是保存结果中按物料列出的数量核对项，r 与
// portions 是这份结果各自依据的配方版本与份数——依据的选取仍由各调用方按
// 自己的历史判断方式决定，本函数不关心依据从何而来：
//   - 创建结果：调用方传原创建请求选定的配方版本与份数（payload），不使用
//     批次后来调整过的当前计划；
//   - 草稿调整结果：调用方传那次成功调整后结果实际采用的版本与份数（编号与
//     版本同时留空、份数为零的沿用值也以结果采用值为准），不使用批次现在的
//     计划代替；
//   - 开始执行结果：调用方传批次开始时最终固定的版本与份数。
//
// 核对规则对三类结果完全一致：
//   - 应投量依据统一由 planRequirements 按 r × portions 计算，因此千分之一克
//     精度与逐物料 maxGramsMilli 上限、份数必须为正等数量规则继续生效；
//   - mats 必须按 r 的物料顺序完整列出每种物料且恰有一项：项数不一致，或某
//     一项物料编号对不上（遗漏、重复、调换顺序或混入其他版本物料）都拒绝；
//   - 每项实投量必须为零、差额必须为应投量的负值：这些结果都是当时尚无投料
//     的快照（创建与调整是无投料草稿，开始是无投料的执行中状态），批次后来
//     再次调整、追加投料或关闭都不改变这份结果，合法的零实投与负差额不能误报。
//
// 数量按精确数值核对而非字符串写法：克数统一经 parseSignedGrams 解析后比较，
// 1 与 1.000、0 与 0.000、-1 与 -1.000 是同一数量；无法解析的写法本身就是
// 损坏。这只放宽读取核对的写法差异，请求内容的幂等匹配仍由写入路径按原文
// 比较，不在本函数处理。
//
// 本函数只负责数量规则本身，返回的错误不带错误分类，且尽量指出问题物料与
// 数量原因（项数不符时没有具体物料可指）；调用方再按各自场景用
// ErrCorruptData 包装，并补充操作类别、请求编号与批次编号。
func validateUnfedRequirements(mats []MaterialRequirement, r *recipeRecord, portions int) error {
	required, err := planRequirements(r, portions)
	if err != nil {
		return err
	}
	if len(mats) != len(required) {
		return fmt.Errorf("保存结果的物料核对项数 %d 与依据配方 %q 版本 %q 的物料项数 %d 不一致，不能遗漏、重复或混入其他版本的物料",
			len(mats), r.RecipeNo, r.Version, len(required))
	}
	for i, item := range required {
		got := mats[i]
		if got.MaterialNo != item.materialNo {
			return fmt.Errorf("保存结果第 %d 项核对物料 %q 与依据版本的物料 %q 不一致（物料遗漏、重复、调换顺序或混入了其他版本的物料）",
				i+1, got.MaterialNo, item.materialNo)
		}
		for _, check := range []struct {
			name string
			raw  string
			want gramsMilli
		}{
			{"应投量", got.RequiredGrams, item.requiredGrams},
			{"实投量", got.ActualGrams, 0},
			{"差额", got.DifferenceGrams, -item.requiredGrams},
		} {
			v, err := parseSignedGrams(check.raw)
			if err != nil {
				return fmt.Errorf("保存结果物料 %q 的%s %q 无法解析为合法克数",
					item.materialNo, check.name, check.raw)
			}
			if v != check.want {
				return fmt.Errorf("保存结果物料 %q 的%s %s 与依据配方 %q 版本 %q 以 %d 份应有的值 %s 不一致",
					item.materialNo, check.name, check.raw, r.RecipeNo, r.Version, portions, check.want)
			}
		}
	}
	return nil
}
