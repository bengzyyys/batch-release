package release

import "fmt"

// plannedMaterial 是数量核对规则对配方中一种物料的应投依据：
// 物料编号及其在当前计划份数下的应投量（千分之一克）。
type plannedMaterial struct {
	materialNo    string
	requiredGrams gramsMilli
}

// planRequirements 是批次“应投量依据”的唯一实现：给定批次采用的配方
// 版本 r 与最终采用的计划份数 portions，按配方中的物料顺序逐项计算
// 每份克数 × 计划份数，返回每种物料的应投量。
//
// 批次查询、关闭结果核对所需的完整数量核对（再加累计实投与差额）统一由
// reconcileMaterials 在本函数结果上继续计算；只需要应投依据的输入校验
// （创建批次、调整草稿）与读取校验（已保存批次的份数/应投量合法性）直接
// 调用本函数：
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

// materialQuantities 是批次中一种物料的完整数量核对结果（千分之一克）：
// 应投量、累计实投量与实投减应投的差额。差额可以为负；物料之间不互相
// 抵消，多种物料的差额相加为零也不代表数量吻合。
type materialQuantities struct {
	materialNo string
	required   gramsMilli
	actual     gramsMilli
	difference gramsMilli
}

// reconcileMaterials 是“批次逐物料数量核对”的唯一实现：给定批次实际
// 绑定的配方版本 r、计划份数 portions 与该批次的全部投料 feedings，
// 按 r 的物料顺序逐项算出每种物料的应投量、累计实投量与差额，每种物料
// 恰有一项，无投料的物料也不消失（实投为零、差额为负属正常结果）。
//
// 批次查询（GetBatch 等经由 buildBatchView）与读取已保存关闭结果时的
// 数量核对（validateCloseBatchRequest）一律调用本函数，对同一批次只
// 可能得到同一份数量依据，维护数量规则时不再有两处需要同步：
//   - 应投量统一由 planRequirements 按 r × portions 计算，千分之一克
//     精度与逐物料 maxGramsMilli 上限、份数必须为正等规则继续生效；
//   - 实投量只累计 feedings 中该物料自己的投料：用与追加投料、读取
//     校验相同的 feedingAccumulator 按物料分别累计，其他物料的投料
//     不参与、不互相抵消，逐物料累计上限同为 maxGramsMilli，恰好
//     达到上限合法，各种物料相加不作为超限依据；
//   - 差额 = 实投 - 应投，逐物料独立计算并保留正负号。
//
// 依据版本与份数一律取调用方给定值：查询用批次实际绑定的版本与当前
// 计划份数；关闭结果核对用原请求所指已关闭批次记录的版本与份数，不能
// 用同编号配方的其他版本或别的批次记录顶替。
func reconcileMaterials(r *recipeRecord, portions int, feedings []feedingRecord) ([]materialQuantities, error) {
	required, err := planRequirements(r, portions)
	if err != nil {
		return nil, err
	}
	// 累计规则与追加投料、validateFeedings 读取校验共用同一个累加器：
	// 只按物料分别累计该批次自己的投料，逐物料判断上限，不做物料间相加。
	acc := newFeedingAccumulator()
	for _, f := range feedings {
		if !acc.add(f.MaterialNo, f.GramsMilli) {
			return nil, fmt.Errorf("物料 %q 的累计实投超出可表示范围", f.MaterialNo)
		}
	}
	out := make([]materialQuantities, 0, len(required))
	for _, item := range required {
		actual := acc.total(item.materialNo)
		out = append(out, materialQuantities{
			materialNo: item.materialNo,
			required:   item.requiredGrams,
			actual:     actual,
			// 实投与应投都在 [0, maxGramsMilli] 内，差值落在 int64 范围内，
			// 不会回绕；欠投为负、超投为正，逐物料保留真实正负值。
			difference: actual - item.requiredGrams,
		})
	}
	return out, nil
}

// compareMaterialQuantities 是“已保存批次结果逐物料数量核对”的唯一比较
// 实现：mats 是保存结果里按物料列出的数量核对项，expected 是按该结果
// 各自依据经 reconcileMaterials 算出的数量核对（按依据版本的物料顺序
// 排列）。无投料快照结果（创建、草稿调整、开始执行：expected 的实投为
// 零、差额为应投量负值）与已关闭批次结果（expected 含全部实际投料的
// 累计）共用这里，差异只在调用方传入什么依据，本函数不关心依据从何而来。
//
// 核对规则：
//   - mats 必须与 expected 项数一致且每项物料编号按位置一致：项数不一致，
//     或某一项物料编号对不上（漏掉物料、重复物料、调换排列顺序或混入其他
//     版本的物料）都拒绝——总差额为零或原始投料完整都不能抵消这些问题；
//   - 每项的应投量、实投量、差额都必须与依据逐项相符：即使原始投料全部
//     正确，只改坏核对差额也属于损坏，不能用重新计算的正确值覆盖损坏结果
//     后继续使用（如何响应损坏由调用方决定，本函数只返回错误）；
//   - 数量按精确数值核对而非字符串写法：克数统一经 parseSignedGrams 解析
//     后比较，1 与 1.000、0 与 0.000、-1 与 -1.000 是同一数量，不因显示
//     写法不同误判；无法解析的写法本身就是损坏。这只放宽读取核对的写法
//     差异，请求内容的幂等匹配仍由写入路径按原文比较，不在本函数处理。
//
// 本函数只负责数量规则本身，返回的错误不带错误分类，且尽量指出问题物料
// 与数量原因（项数不符时没有具体物料可指）；调用方再按各自场景用
// ErrCorruptData 包装，并补充操作类别、请求编号与批次编号。
func compareMaterialQuantities(mats []MaterialRequirement, expected []materialQuantities) error {
	if len(mats) != len(expected) {
		return fmt.Errorf("保存结果的物料核对项数 %d 与数量依据的物料项数 %d 不一致，不能漏掉物料、重复物料或改变排列顺序",
			len(mats), len(expected))
	}
	for i, item := range expected {
		got := mats[i]
		if got.MaterialNo != item.materialNo {
			return fmt.Errorf("保存结果第 %d 项核对物料 %q 与数量依据的物料 %q 不一致（物料遗漏、重复、调换顺序或混入了其他版本的物料）",
				i+1, got.MaterialNo, item.materialNo)
		}
		for _, check := range []struct {
			name string
			raw  string
			want gramsMilli
		}{
			{"应投量", got.RequiredGrams, item.required},
			{"实投量", got.ActualGrams, item.actual},
			{"差额", got.DifferenceGrams, item.difference},
		} {
			v, err := parseSignedGrams(check.raw)
			if err != nil {
				return fmt.Errorf("保存结果物料 %q 的%s %q 无法解析为合法克数",
					item.materialNo, check.name, check.raw)
			}
			if v != check.want {
				return fmt.Errorf("保存结果物料 %q 的%s %s 与数量依据应有的值 %s 不一致",
					item.materialNo, check.name, check.raw, check.want)
			}
		}
	}
	return nil
}

// validateUnfedRequirements 核对“无投料批次快照”结果（创建批次、草稿调整
// 与开始执行三类已保存成功请求在读取台账时共用）的逐物料数量：这些结果
// 都是当时尚无投料的快照，数量依据即按 r × portions 计算、实投为零、
// 差额为应投量负值的核对——直接以空投料列表调用 reconcileMaterials 得到，
// 再与保存结果逐项比较。依据的选取仍由各调用方按自己的历史判断方式决定
// （创建用原请求选定的版本与份数；草稿调整用那次结果实际采用的版本与
// 份数，含沿用值；开始执行用批次开始时最终固定的版本与份数），本函数不
// 关心依据从何而来。项数/顺序、零实投、负差额、精确克数、精度与上限等
// 全部规则统一由 reconcileMaterials 与 compareMaterialQuantities 实现，
// 与关闭结果的核对共用同一处，不再单独维护一份。
func validateUnfedRequirements(mats []MaterialRequirement, r *recipeRecord, portions int) error {
	expected, err := reconcileMaterials(r, portions, nil)
	if err != nil {
		return err
	}
	return compareMaterialQuantities(mats, expected)
}
