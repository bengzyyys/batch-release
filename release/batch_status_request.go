package release

import (
	"fmt"
)

// statusTransitionRequest 概括读取台账时“批次状态变更类”已保存成功请求
// （开始执行 startBatch、关闭批次 closeBatch）的核对上下文。
//
// 四类批次操作共有的归属与完整性规则（原提交解析、保存结果完整、原批次
// 存在、结果批次归属）统一由内嵌的 batchRequestCheck 各步骤核对一份，这里
// 只按状态变更自己的先后顺序组织这些步骤，并补两类结果共有的状态与计划
// 业务判断：
//   - 原批次当前状态必须仍在该类结果允许的范围内（开始结果允许执行中或
//     已关闭；关闭结果只允许已关闭）——状态只前进不回退；
//   - 配方依据是该批次开始执行时最终固定的编号与版本，名称取这个已登记
//     版本，份数也取开始时固定的计划。草稿最初采用第一版、五份，开始前
//     改成第二版、三份时，开始结果与关闭结果都对应第二版、三份，不能
//     沿用创建草稿时的旧计划。
//
// 两类结果的区别不在这层处理：开始结果仍是开始成功当时的执行中状态、
// 空投料及相应数量核对，批次后来追加投料或关闭不会使它失效；关闭结果
// 仍是已关闭状态，并保留关闭确认的全部投料及数量核对。状态准入、投料
// 保留与数量核对的差异由 validateStartBatchRequest、
// validateCloseBatchRequest 在本结构给出的共有判定之后各自核对。
//
// 各 label 字段保留两类操作各自的错误措辞：整理成一份逻辑后，错误仍带
// 操作类别、请求编号、能确定的批次编号及具体不符原因，不会退化成无法
// 定位的通用提示。
type statusTransitionRequest struct {
	batchRequestCheck
	// allowedStatusDesc 描述原批次当前必须处于的状态（执行中或已关闭 /
	// 仍为已关闭），用于状态不符时的错误说明。
	allowedStatusDesc string
	// recipeBasisDesc 与 portionsBasisDesc 描述配方与份数的核对依据：
	// 两类结果的依据实际相同（都是开始时固定的批次记录），只是保留各自
	// 原来的原因措辞。
	recipeBasisDesc   string
	portionsBasisDesc string
	// allowExecuting 与 allowClosed 是“原请求所指批次当前允许的状态”：
	// 开始结果要求原批次当前为执行中或已关闭；关闭结果要求原批次仍为已关闭。
	allowExecuting bool
	allowClosed    bool
}

// startBatchRequestCheck 是已保存开始执行请求的核对配置：原批次当前
// 为执行中或已关闭即可，配方与份数表述为“批次开始时确定”。
var startBatchRequestCheck = statusTransitionRequest{
	batchRequestCheck: batchRequestCheck{
		opLabel:      "开始执行请求",
		resultLabel:  "开始结果",
		successLabel: "开始成功",
	},
	allowedStatusDesc: "不是执行中或已关闭",
	recipeBasisDesc:   "批次开始时确定的配方",
	portionsBasisDesc: "批次开始时确定的份数",
	allowExecuting:    true,
	allowClosed:       true,
}

// closeBatchRequestCheck 是已保存关闭请求的核对配置：原批次必须仍为
// 已关闭，配方与份数表述为“批次实际绑定/实际份数”。
var closeBatchRequestCheck = statusTransitionRequest{
	batchRequestCheck: batchRequestCheck{
		opLabel:      "关闭请求",
		resultLabel:  "关闭结果",
		successLabel: "关闭成功",
	},
	allowedStatusDesc: "不是已关闭",
	recipeBasisDesc:   "批次实际绑定的配方",
	portionsBasisDesc: "批次实际份数",
	allowExecuting:    false,
	allowClosed:       true,
}

// resolve 核对开始执行/关闭两类已保存成功请求。四类操作共有的步骤都委托
// 给内嵌 batchRequestCheck 的同名方法（只维护一份），这里按状态变更原有
// 的先后顺序组织，并在原批次存在后、结果归属确认前后插入状态准入与批次
// 开始时固定的配方/份数核对。成功时返回指向原请求所指批次的记录 b、该
// 批次绑定的已登记配方版本 r，以及解析后的保存结果视图。
//
// 检查顺序与整理前一致：原提交内容可解析 → 保存结果完整 → 原批次存在 →
// 当前状态准入 → 结果批次编号归属 → 结果配方编号/版本/名称与批次开始时
// 固定的记录一致 → 计划份数一致。
//
// 至此共有与状态变更规则结束：开始结果的执行中状态、空投料与零实投
// 核对，以及关闭结果的已关闭状态、全部投料保留与含实投的完整数量核对，
// 都不在这里判断，由调用方按各自结果含义继续核对。
func (cfg statusTransitionRequest) resolve(st *persistedState, reqNo string, req *requestRecord) (*batchRecord, *recipeRecord, *BatchView, error) {
	batchNo, err := cfg.parseRequestBatch(reqNo, req)
	if err != nil {
		return nil, nil, nil, err
	}
	result, err := cfg.requireBatchResult(reqNo, batchNo, req)
	if err != nil {
		return nil, nil, nil, err
	}
	// 原请求所指的批次必须存在：批次没有了，保存的结果不能单独作为成功
	// 的依据。开始/关闭都只会让状态前进，没有退回草稿的路径。
	b, err := cfg.requireExistingBatch(st, reqNo, batchNo)
	if err != nil {
		return nil, nil, nil, err
	}
	// 当前状态必须在允许范围内：开始结果允许执行中或已关闭（开始后状态
	// 只前进，被改回草稿即不一致）；关闭结果只允许已关闭。
	if !((cfg.allowExecuting && b.Status == StatusExecuting) ||
		(cfg.allowClosed && b.Status == StatusClosed)) {
		return nil, nil, nil, fmt.Errorf("%w: %s %q 对应的批次 %q 当前状态为 %s，%s，保存的%s与已确认记录不一致",
			ErrCorruptData, cfg.opLabel, reqNo, batchNo, b.Status, cfg.allowedStatusDesc, cfg.resultLabel)
	}
	// 保存结果必须指向原请求指定的批次：另一批次即使采用相同配方、份数
	// 和投料，其结果也不能顶替。
	if err := cfg.requireResultOwnedByBatch(reqNo, batchNo, result); err != nil {
		return nil, nil, nil, err
	}
	// 配方绑定与计划份数对应批次开始时最终固定的版本与份数。开始执行后
	// 两者固定，批次当前记录即开始时的计划；草稿阶段调整过计划的，以这
	// 份最终选定为准（草稿最初 v1、5 份，开始前 v2、3 份时，两类结果
	// 都只能是 v2、3 份）。批次已在前面的整表校验中确认绑定版本存在，
	// 这里仍防一手。
	r := findRecipe(st, b.RecipeNo, b.RecipeVersion)
	if r == nil {
		return nil, nil, nil, fmt.Errorf("%w: %s %q 对应的批次 %q 绑定的配方 %q 版本 %q 未登记",
			ErrCorruptData, cfg.opLabel, reqNo, batchNo, b.RecipeNo, b.RecipeVersion)
	}
	if result.RecipeNo != b.RecipeNo || result.RecipeVersion != b.RecipeVersion ||
		result.RecipeName != r.Name {
		return nil, nil, nil, fmt.Errorf("%w: %s %q（批次 %q）保存结果的配方编号、版本或名称与%s %q 版本 %q 不一致",
			ErrCorruptData, cfg.opLabel, reqNo, batchNo, cfg.recipeBasisDesc, b.RecipeNo, b.RecipeVersion)
	}
	if result.PlannedPortions != b.PlannedPortions {
		return nil, nil, nil, fmt.Errorf("%w: %s %q（批次 %q）保存结果的计划份数 %d 与%s %d 不一致",
			ErrCorruptData, cfg.opLabel, reqNo, batchNo, result.PlannedPortions, cfg.portionsBasisDesc, b.PlannedPortions)
	}
	return b, r, result, nil
}
