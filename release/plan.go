package release

import "fmt"

// 应投量核对的唯一数量规则维护点：
//
// 每种物料的应投量由批次实际采用的配方版本中每份克数乘以计划份数确定，
// 精确到千分之一克，单种物料上限为 maxGramsMilli（9223372036854775.807 克）。
// 创建批次、调整草稿、读取已保存批次与构造批次查询结果（进而覆盖开始执行、
// 关闭与幂等重放返回的视图）都经过 planRequiredGrams 逐物料计算与判断，
// 规则只在这里维护一次，不再散落在各入口分别循环相乘。
//
// 数量约束（所有场景一致，不随入口改变）：
//   - 计划份数必须为正整数——份数为零或负数的已保存批次即数据损坏，
//     也不能据此算出零或负的应投量；调整草稿时份数传 0 表示沿用当前份数，
//     走到这里的必是这次提交最终采用的正份数；
//   - 相乘结果必须能用千分之一克（int64 个 milli）精确表示：每份克数本身
//     已是千分之一克的整数倍，整数相乘不引入更小的单位，不存在舍入；
//     结果超过 maxGramsMilli（9223372036854775.807 克）时返回 planError，
//     不能通过舍入、截断或减少份数接受原本超限的计划；
//   - 上限按每种物料分别判断：只乘该物料自己的每份克数与份数，不与同批次
//     其他物料的应投量相加，多种物料应投量相加超上限不影响各项合法性；
//     恰好等于上限合法，再大即不可接受。计算不回绕、不截断、不舍入。
//
// 同编号的其他版本每份克数可能不同，应投量只按实际采用/绑定的版本 r 计算，
// 不能用其他版本作为判断或计算依据。

// planError 标记一个已解析计划在应投量上不合法：份数非正，或某物料
// （在配方物料列表中的索引为 index）按每份克数 × 份数算出的应投量
// 无法精确表示或超过上限。它只携带上下文，不带任何错误哨兵——是否归类为
// ErrInvalidInput（创建/调整提交）还是 ErrCorruptData（读取已保存批次）
// 由对应的入口薄包装决定。
type planError struct {
	portions int
	index    int // -1 表示份数本身非正，尚未定位到具体物料
	cause    error
}

func (e *planError) Error() string {
	return e.cause.Error()
}

// planRequiredGrams 按配方版本 r 的物料顺序，逐项计算每份克数 × 计划份数
// 的应投量并做统一校验。合法时返回与 r.Materials 等长、同序的应投量切片
// （与记录隔离的新切片）；份数非正或任一物料应投量溢出 int64 个千分之一克
// 时，返回指向该物料的 *planError。
//
// 这是创建/调整提交、台账读取校验与查询视图构造共用的“计算 + 校验”核心：
// 提交路径一次计算同时用于拒绝超限输入和构造返回视图，不再先校验丢弃结果、
// 稍后重新相乘；查询列应投量与台账合法性出自同一套规则。
func planRequiredGrams(r *recipeRecord, portions int) ([]gramsMilli, error) {
	if portions <= 0 {
		return nil, &planError{portions: portions, index: -1, cause: fmt.Errorf("份数必须为正整数")}
	}
	required := make([]gramsMilli, len(r.Materials))
	for i, m := range r.Materials {
		v := int64(m.GramsMilli)
		if v > (1<<63-1)/int64(portions) {
			return nil, &planError{
				portions: portions,
				index:    i,
				cause: fmt.Errorf("应投量超出上限 %s 克：%s 克 × %d 份无法精确表示",
					maxGramsMilli, m.GramsMilli, portions),
			}
		}
		required[i] = gramsMilli(v * int64(portions))
	}
	return required, nil
}

// invalidPlanCause 把创建批次或调整草稿时算出的计划错误包装成本次提交的
// 输入错误（ErrInvalidInput），并按原格式补充物料、配方编号、版本与份数
// 上下文。份数非正的情形仍按各入口原有的“计划份数必须为正整数”消息处理，
// 理论上不会走到这里（调用方先拦截），兜底给出同样明确的输入错误。
func invalidPlanCause(r *recipeRecord, err *planError) error {
	if err.index < 0 {
		return fmt.Errorf("%w: 计划份数必须为正整数，得到 %d", ErrInvalidInput, err.portions)
	}
	m := r.Materials[err.index]
	return fmt.Errorf("%w: 物料 %q 按配方 %q 版本 %q 以 %d 份计算的应投量不合法: %v",
		ErrInvalidInput, m.MaterialNo, r.RecipeNo, r.Version, err.portions, err)
}

// validateBatchPlan 检查一个已保存批次的计划份数与按绑定配方版本算出的
// 应投量，属于整份台账读取时的完整性校验：不合法一律归类为 ErrCorruptData。
// 实际的逐物料计算与上限判断在 planRequiredGrams，这里只决定错误分类与
// 损坏场景的错误文案：错误信息包含批次编号、计划份数；份数非正时指出
// “不是正整数”，应投量超限时还包含对应物料及批次绑定的配方编号、版本号。
// 归属以批次实际绑定版本为准，计划合法与否与投料无关。
func validateBatchPlan(b *batchRecord, r *recipeRecord) error {
	_, err := planRequiredGrams(r, b.PlannedPortions)
	if err == nil {
		return nil
	}
	pe := err.(*planError)
	if pe.index < 0 {
		return fmt.Errorf("%w: 批次 %q 的计划份数 %d 不是正整数",
			ErrCorruptData, b.BatchNo, b.PlannedPortions)
	}
	m := r.Materials[pe.index]
	return fmt.Errorf("%w: 批次 %q（计划份数 %d）物料 %q 按绑定配方 %q 版本 %q 的应投量不合法: %v",
		ErrCorruptData, b.BatchNo, b.PlannedPortions, m.MaterialNo,
		b.RecipeNo, b.RecipeVersion, pe)
}
