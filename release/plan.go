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

// validateUnfedResultMaterials 是“尚未投料的批次结果”数量核对的唯一实现：
// 首次创建、草稿调整与首次开始执行成功时，批次都还没有任何投料，三类保存
// 结果的数量核对遵循同一套规则——物料按结果依据的配方版本顺序完整列出，
// 每种物料恰有一项，应投量等于计划应投量（每份克数 × 当时份数，由调用方
// 按各自依据确定版本与份数后经 planRequirements 算出，通过 required 传入），
// 实投量为零，差额为应投量的负值。
//
// 结果依据的配方版本与份数不由本函数判断：创建取原提交内容选定的版本与
// 份数，草稿调整取结果实际采用的版本与份数（沿用值也在结果中），开始执行
// 取批次最终固定的记录——各调用方确定后把算好的 required 交给这里统一核对。
// 这里只负责核对保存结果与这组应有数量是否一致：一项不多、一项不少，不能
// 遗漏、重复、调换顺序或混入其他版本的物料。数量按精确克数核对而非字符串
// 写法：1 与 1.000、0 与 0.000、-1 与 -1.000 分别相同，合法的零实投与负
// 差额不是损坏；无法解析或与应有值不符的数量都是数据损坏。
//
// opDesc 是操作类别（如 "创建批次请求"），与 reqNo、batchNo 一起写入错误
// 信息；能确定问题物料时一并写明物料编号与数量原因。
func validateUnfedResultMaterials(opDesc, reqNo, batchNo string, required []plannedMaterial, got []MaterialRequirement) error {
	if len(got) != len(required) {
		return fmt.Errorf("%w: %s %q（批次 %q）保存结果的物料核对项数 %d 与应有项数 %d 不一致，不能遗漏、重复或混入其他版本的物料",
			ErrCorruptData, opDesc, reqNo, batchNo, len(got), len(required))
	}
	for i, item := range required {
		rm := got[i]
		if rm.MaterialNo != item.materialNo {
			return fmt.Errorf("%w: %s %q（批次 %q）保存结果第 %d 项核对物料 %q 与应有物料 %q 不一致（顺序被调换、遗漏或混入其他版本物料）",
				ErrCorruptData, opDesc, reqNo, batchNo, i+1, rm.MaterialNo, item.materialNo)
		}
		// 应投量取计划值，实投量必为零，差额必为应投量的负值：尚未投料的
		// 结果里，真实的零实投与负差额是合法内容，按精确克数核对即可。
		for _, check := range []struct {
			name string
			got  string
			want gramsMilli
		}{
			{"应投量", rm.RequiredGrams, item.requiredGrams},
			{"实投量", rm.ActualGrams, 0},
			{"差额", rm.DifferenceGrams, -item.requiredGrams},
		} {
			gotVal, err := parseSignedGrams(check.got)
			if err != nil {
				return fmt.Errorf("%w: %s %q（批次 %q）保存结果物料 %q 的%s %q 不合法",
					ErrCorruptData, opDesc, reqNo, batchNo, item.materialNo, check.name, check.got)
			}
			if gotVal != check.want {
				return fmt.Errorf("%w: %s %q（批次 %q）保存结果物料 %q 的%s %s 与应有值 %s 不一致",
					ErrCorruptData, opDesc, reqNo, batchNo, item.materialNo, check.name, check.got, check.want)
			}
		}
	}
	return nil
}
