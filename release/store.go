package release

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"syscall"
)

const (
	stateFileName = "ledger.json"
	lockFileName  = "ledger.lock"
	stateVersion  = 1
)

// Store 是一个本地配方批次台账，数据保存在调用方指定的目录中。
// 不同目录互相独立；多个 Store 可以安全并发提交（进程内互斥、进程间文件锁）。
type Store struct {
	// dir 是打开时确定的绝对路径。即使传入相对目录，也在 Open 时按当时的
	// 工作目录解析并固定下来：此后调用方切换进程工作目录，本对象的读取与
	// 保存仍指向最初打开的位置，绝不会按新工作目录重新解释这个相对路径。
	dir      string
	lockFile *os.File
	mu       sync.Mutex
	state    *persistedState
}

// Open 打开（或首次使用）位于 dir 的台账。
// 目录不存在时会创建；目录中没有台账文件时得到空台账；
// 台账文件已存在但没有任何内容（零字节，无论原本就是空文件还是使用中被截断）
// 时按损坏处理；已有台账文件无法读取、解析，存在两条配方编号与版本号
// 完全相同的配方记录，存在两条批次编号完全相同的批次记录，
// 任一已保存配方版本不包含任何物料（物料列表为空数组、null 或字段缺失，
// 即使名称、编号、版本号齐全且内容能正常解析；含尚未被任何批次引用的版本），
// 任一配方版本的任一物料每份克数不是正数，
// 任一配方版本内同一物料编号出现多次，
// 任一批次绑定的配方版本未登记，
// 任一批次的状态不是精确的 "draft"、"executing"、"closed" 之一
// （缺失、为空、null、其他字符串，或大小写、前后空格与合法值不同），
// 任一批次的计划份数不是正整数，或按其绑定的配方版本计算时某物料的
// 应投量（每份克数 × 计划份数）无法精确表示到千分之一克或超过
// 9223372036854775.807 克，
// 任一批次仍为草稿状态却带有投料记录，
// 任一批次已保存投料的登记序号与记录在列表中的位置不一致
// （第一条不为 1、零或负数、重号、漏号、跳号，或调换记录位置后
// 未同步序号；不同物料共用批次序号、不同批次各自从 1 开始），
// 已保存投料的物料编号不属于该批次绑定的配方版本，
// 或已保存的投料数量非法（单条不是正数，或同一批次同一物料累计实投
// 超过 9223372036854775.807 克），
// 或任一已保存的成功配方登记请求的结果缺失、为 null、为空对象、无法读成
// 完整配方结果，或原请求登记的版本不存在，或保存结果、原提交内容与实际登记
// 版本不一致（配方编号、版本号、名称、物料项数、排列顺序、各项物料编号与每份
// 克数有任何差异；同编号的其他完整版本不能顶替，名称或用量相同也不算同一
// 版本；每份克数按精确数值核对，1.000 与 1 不算差异），
// 或任一已保存的成功创建批次请求的结果缺失、为 null、为空对象、无法读成
// 完整批次结果，或原提交内容无法解析或不符合创建要求，或原请求对应的批次、
// 原选配方版本不存在，或保存结果与原提交不一致（批次编号、配方编号、版本、
// 名称与创建时份数对不上，状态不是草稿，混入了后来的投料，或逐物料核对项的
// 顺序、应投量、零实投、负差额对不上；另一批次或同编号配方的其他版本即使
// 名称、物料与数量完全相同也不能顶替；批次后来的调整、开始、投料、关闭不
// 影响首次创建结果的合法性），
// 或任一已保存的成功投料请求的结果缺失、为 null、为空对象，或其登记
// 序号在原请求对应的批次中不存在，或结果的物料编号、实际克数、投料
// 时间（同一时刻不区分时区写法）、登记人与该序号的实际投料或原提交
// 内容不一致（其他批次的同序号投料不能作为对应记录，原批次不存在时
// 保存的结果不能单独作为登记成功的依据），
// 或任一已保存的成功开始执行请求的结果缺失、为 null、为空对象、无法读成
// 批次结果，或保存结果与原请求所指批次第一次开始执行时的内容不一致（原
// 批次必须存在且当前为执行中或已关闭；结果要对应批次开始时确定的配方
// 编号、版本、名称与计划份数，状态为执行中，投料列表为空，各物料按绑定
// 配方的顺序完整列出，应投量为每份克数 × 计划份数、实投量为零、差额为
// 应投量的负值；另一批次的结果不能顶替，批次后来的投料与关闭不能混入，
// 数量按精确克数核对），
// 或任一已保存的成功关闭请求的结果缺失、为 null、为空对象、无法读成
// 批次结果，或保存结果与原请求所指的已关闭批次不一致（原批次必须存在
// 并仍为已关闭；结果要对应批次实际绑定的配方编号、版本、名称与计划份数，
// 保留全部投料及其登记顺序，投料的序号、物料、数量、时间、登记人逐项
// 一致，逐物料的应投量、实投量与差额与批次相符；另一批次的结果不能顶替，
// 数量按精确克数核对，时间按同一时刻核对）时，返回 ErrCorruptData（可用
// errors.Is 判断），不会当成空台账继续保存，也不会返回可继续使用的
// 台账对象。
//
// dir 可以是相对目录：其位置只按 Open 调用时的工作目录解析一次并固定。
// 打开之后即使进程切换了工作目录，本对象的查询与保存仍始终指向最初打开
// 的目录，不会按新工作目录重新解释相对路径；台账操作本身也不会改变调用
// 方的工作目录。切换工作目录后另行 Open 同一相对目录，得到的是绑定新位
// 置的独立对象，与此前的对象互不影响。
func Open(dir string) (*Store, error) {
	if dir == "" {
		return nil, errors.New("数据位置不能为空")
	}
	// 先按“打开那一刻”的工作目录把位置解析成绝对路径并固定。后续每次
	// 查询与写入都会重新读取台账文件，若仍持有相对路径，调用方一旦切换
	// 工作目录，相对路径就会指向别处——可能把已登记批次查成不存在，或把
	// 变更读写到另一个同名台账。解析为绝对路径后，本对象始终绑定最初打
	// 开的位置。filepath.Abs 只做词法解析、不访问文件系统，目录尚不存在
	// （首次使用会自动创建）时也能得到稳定结果。
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("解析数据位置 %q 失败: %w", dir, err)
	}
	if err := os.MkdirAll(absDir, 0o755); err != nil {
		return nil, fmt.Errorf("创建数据位置 %q 失败: %w", absDir, err)
	}
	info, err := os.Stat(absDir)
	if err != nil {
		return nil, fmt.Errorf("读取数据位置 %q 失败: %w", absDir, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("数据位置 %q 不是目录", absDir)
	}

	lockPath := filepath.Join(absDir, lockFileName)
	lf, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("打开锁文件失败: %w", err)
	}

	s := &Store{dir: absDir, lockFile: lf}
	if err := s.load(); err != nil {
		lf.Close()
		return nil, err
	}
	return s, nil
}

// Close 释放台账占用的文件锁。
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lockFile == nil {
		return nil
	}
	err := s.lockFile.Close()
	s.lockFile = nil
	return err
}

func (s *Store) lockExclusive() error {
	if s.lockFile == nil {
		return errors.New("台账已关闭")
	}
	if err := syscall.Flock(int(s.lockFile.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("锁定台账失败: %w", err)
	}
	return nil
}

func (s *Store) lockShared() error {
	if s.lockFile == nil {
		return errors.New("台账已关闭")
	}
	if err := syscall.Flock(int(s.lockFile.Fd()), syscall.LOCK_SH); err != nil {
		return fmt.Errorf("锁定台账失败: %w", err)
	}
	return nil
}

func (s *Store) unlock() {
	_ = syscall.Flock(int(s.lockFile.Fd()), syscall.LOCK_UN)
}

func (s *Store) load() error {
	data, err := os.ReadFile(filepath.Join(s.dir, stateFileName))
	if err != nil {
		if os.IsNotExist(err) {
			s.state = &persistedState{Version: stateVersion, Requests: map[string]*requestRecord{}}
			return nil
		}
		return fmt.Errorf("%w: 读取台账文件失败: %v", ErrCorruptData, err)
	}
	if len(data) == 0 {
		// 区分“没有台账文件”与“已有文件但内容为空”：文件不存在才算首次使用，
		// 零字节文件（原本就为空或使用中被截断）一律按损坏处理——既不能当成
		// 空台账补写，也不能凭内存中的旧记录还原，调用方必须明确知道无法读取。
		return fmt.Errorf("%w: 台账文件 %q 没有内容", ErrCorruptData, filepath.Join(s.dir, stateFileName))
	}
	var st persistedState
	if err := json.Unmarshal(data, &st); err != nil {
		return fmt.Errorf("%w: 台账文件解析失败: %v", ErrCorruptData, err)
	}
	if st.Version != stateVersion {
		return fmt.Errorf("%w: 台账版本 %d 不受支持", ErrCorruptData, st.Version)
	}
	if err := validateState(&st); err != nil {
		return err
	}
	if st.Requests == nil {
		st.Requests = map[string]*requestRecord{}
	}
	s.state = &st
	return nil
}

// validateState 检查台账的引用完整性与数量合法性：
//   - 配方由配方编号与版本号共同识别，同一台账内每组编号+版本号只能
//     对应一条配方记录。只要同组出现第二条记录，无论名称、物料、每份克数
//     是否相同（即使内容完全一致），整份台账即视为损坏：不能挑第一条或
//     最后一条、不能合并物料、也不能自行删除一条后继续——重复记录会让
//     查询结果与批次的应投依据取决于文件中的排列顺序。重复版本即使尚未
//     被任何批次引用也一样拒绝，其他配方与批次完整也不能放行本次读取。
//   - 批次由批次编号唯一识别，同一台账内一个编号只能对应一条批次记录。
//     同一编号出现第二条记录，无论绑定的配方版本、计划份数、状态是否
//     相同（即使内容完全一致），整份台账即视为损坏：不能挑第一条或最后
//     一条、不能合并投料、不能自动改号或删除一条后继续——重复记录会让
//     同一编号查到的配方、投料与数量核对取决于文件中的排列顺序。其他
//     批次完整、投料数量吻合，或调用方只想查看另一个正常批次，都不能
//     放行本次读取；不同编号的批次绑定同一配方版本不受影响。
//   - 每个已保存配方版本必须至少包含一种物料——与登记配方时的规则一致。
//     物料列表保存成空数组、null 或字段缺失，解析后都是空列表，属于同一
//     种损坏：没有物料就没有任何用量依据，采用它的批次得不到任何数量核对项。
//     任一版本（包括尚未被任何批次使用的版本）没有物料，整份台账即视为
//     损坏，即使其名称、编号与版本号齐全、台账内容能正常解析也不能接受：
//     不能补造物料、不能删除问题版本，也不能用同一配方编号下的其他完整
//     版本顶替。其他配方与批次完整、调用方只查询正常记录，或采用该版本
//     的批次份数为正、状态合法且尚未投料（没有任何数量差额），都不能放行。
//     没有登记任何配方的空台账与此不同，仍按空台账正常使用。
//   - 每个配方版本的每种物料，每份克数必须是正数——与登记时的规则一致。
//     任一版本（包括尚未被任何批次使用的版本）的任一物料为零或负数，
//     整份台账即视为损坏：不能改成最小用量、取绝对值、删除问题物料
//     或改用其他版本后继续，其他配方完整或批次数量恰好吻合也不能放行。
//   - 同一配方版本的物料列表内，每个物料编号只能出现一次——与登记时的
//     规则一致。任一版本（包括尚未被任何批次使用的版本）内同一物料编号
//     出现多次，整份台账即视为损坏：不能合并重复项、相加克数、丢弃其中
//     一条或由查询者挑一项作为依据，即使两条克数完全相同也不能放行；
//     不同版本、不同配方各自使用同一物料编号不受影响。
//   - 每个批次记录的配方编号与版本号必须共同指向一个已登记的配方版本。
//     任一批次找不到对应版本，整份台账即视为损坏——不能改用同编号的其他版本，
//     也不能按名称或物料内容替换，即使其他批次仍然完整也不能放行本次读取。
//   - 每个已保存批次的计划份数必须为正整数，且按该批次实际绑定的配方版本
//     计算，每种物料的应投量（每份克数 × 计划份数）都必须能精确表示到
//     千分之一克、且不超过 maxGramsMilli（9223372036854775.807 克）。
//     任一批次份数为零或负数，或任一物料的应投量无法精确表示/超出上限，
//     整份台账即视为损坏：不区分草稿、执行中还是已关闭，也不取决于本次
//     查询哪个批次——不能自动减少份数、改选其他配方版本或删除问题批次，
//     即使其他批次完整也不能放行。上限按每种物料分别判断：多种物料的
//     应投量相加超过上限不构成非法，单种物料恰好等于上限仍合法。
//   - 每个已保存批次的状态必须精确对应 draft、executing、closed 三个
//     字符串之一。状态字段缺失、为空或为 null，或保存成 "ready" 等其他
//     字符串，整份台账即视为损坏：不做大小写归一化，也不去除前后空格，
//     "Draft"、" draft " 与空字符串一样不合法。不能根据该批次有没有投料
//     替它猜一个状态、不能补成草稿，也不能删除该批次或跳过它继续返回
//     其他批次与配方——状态无法识别时，草稿带投料、状态机准入、查询
//     视图等所有依赖状态的规则都无从套用。即使配方版本与计划份数都正常、
//     尚未投料或投料数量恰好吻合，也不能放行；其他批次完整或本次只查询
//     别的批次、配方同样不能绕过。
//   - 草稿批次不能带有任何投料记录：登记投料只允许操作执行中的批次，
//     草稿尚未开始执行，出现投料说明保存内容自相矛盾。任一草稿批次包含
//     至少一条投料，整份台账即视为损坏——即使这些投料的物料都属于绑定
//     版本、克数均为合法正数、累计实投恰好等于应投，也不能接受；不能
//     丢弃投料后当作干净草稿，不能把批次自动改为执行中或已关闭，也不
//     能替调用方改选配方版本来继续读取。台账里另有正常批次、调用方只
//     查询其他配方或批次，都不能绕过这项矛盾。没有投料的草稿不受影响；
//     执行中尚未投料、已关闭批次没有投料或仍有欠投超投都属正常。
//   - 每条已保存的投料数量必须是正数；同一批次内同一物料的累计实投
//     不得超过 maxGramsMilli（9223372036854775.807 克）。任一记录为零或
//     负数，或任一物料累计超限，整份台账即视为损坏——负数记录即使能被
//     正数抵消回范围内也不接受，超限也不能截断或忽略后继续。
//   - 每个批次的投料按保存位置共用一条从 1 开始的登记序列：第一条序号
//     必须为 1，此后每条依次加 1，且保存序号必须与该记录在列表中的位置
//     一致。不同物料的投料共用同一序列、不按物料各自编号；不同批次各自
//     从 1 开始。任一条记录的序号为零或负数、与前一条不连续（漏号、
//     跳号）、与其他记录重号，或记录调换了位置却未同步序号，整份台账即
//     视为损坏：即使物料归属与数量全部合法也不能接受，不能按自带序号或
//     投料时间重排、不能补号、不能删除问题记录后继续。投料时间由调用方
//     填写、从不参与排序，时间相同或乱序不能作为序号有误的依据。空投料
//     列表不构成序号错误，仍按批次状态规则处理。
//   - 每条已保存投料的物料编号必须属于该批次绑定的配方版本。归属以批次
//     实际绑定的版本为准：物料只出现在同编号的其他版本或其他配方中，
//     不能作为接受依据；也不能改选版本、补入物料或丢弃问题投料后继续。
//   - 每条已保存的成功请求都必须能重放回它第一次成功时的结果。配方登记
//     请求的保存结果必须与原提交内容、台账中实际登记的版本三者一致
//     （配方编号、版本号、名称、物料项数、排列顺序、各项物料编号与每份
//     克数；克数按精确数值核对，1.000 与 1 是同一用量）；创建批次请求的
//     保存结果必须是原请求第一次创建成功时的草稿批次结果——原批次必须仍
//     存在，结果对应原提交选定的已登记版本（编号、版本、名称取该版本）与
//     创建时份数，状态为草稿、投料为空，逐物料按原选版本顺序列出应投量
//     （每份克数 × 创建时份数）、零实投与负差额，批次后来的调整、开始、
//     投料、关闭不能混入，另一批次或同编号其他版本不能顶替；投料请求的保存
//     结果必须对应原请求批次中同序号的实际投料并符合原提交内容；开始
//     执行请求的保存结果必须是原请求所指批次（必须存在且当前为执行中
//     或已关闭）第一次开始执行时的批次结果——批次开始时确定的配方绑定
//     与计划份数、执行中状态、空投料列表，以及逐物料应投量 = 每份克数 ×
//     计划份数、实投量为零、差额为应投量的负值，批次后来的投料与关闭
//     不能混入；关闭请求的保存结果必须是原请求所指批次（必须存在并仍为
//     已关闭）第一次关闭时确认的完整批次结果——配方绑定、计划份数、全部
//     投料（登记顺序，序号、物料、数量、时间、登记人逐项一致，不少一条、
//     不多一条、不合并同物料记录）与逐物料的应投量、实投量、差额都与该
//     批次实际记录相符，另一批次的结果即使配方与投料数量相同也不能顶替。任一请求的保存结果
//     缺失、为 null、为空对象、无法解析或对应关系不成立（原请求
//     登记的版本/批次或序号不存在、内容对不上），整份台账即视为损坏：
//     不能仅凭保存结果当作登记成功返回，不能用同编号的其他版本或其他批次
//     的同序号记录顶替，也不能删除请求、补造记录或重新执行原登记。该检查
//     覆盖所有已保存请求，与本次查询或写入哪条记录无关。
func validateState(st *persistedState) error {
	// 先按“配方编号 + 版本号”唯一标识遍历全部配方记录：同组出现第二条
	// 记录即数据损坏，必须先于一切按标识查找的校验拒绝——否则重复记录
	// 会让 findRecipe 总是返回排在前面的一条，查询结果与批次应投依据都
	// 取决于文件排列顺序。完全相同的内容也不是合法重复。
	versions := make(map[recipeKey]bool, len(st.Recipes))
	for _, r := range st.Recipes {
		if r == nil {
			return fmt.Errorf("%w: 台账中存在空的配方记录", ErrCorruptData)
		}
		key := recipeKey{r.RecipeNo, r.Version}
		if versions[key] {
			return fmt.Errorf("%w: 配方编号 %q 版本号 %q 的配方记录重复",
				ErrCorruptData, r.RecipeNo, r.Version)
		}
		versions[key] = true
	}
	for _, r := range st.Recipes {
		// 每个已保存配方版本必须至少包含一种物料——与登记配方时的规则一致：
		// 没有物料就没有任何用量依据，采用它的批次得不到任何数量核对项。
		// 物料列表保存成空数组、null 或字段缺失，解析后都是空列表，属于同一
		// 种损坏。即使名称、编号、版本号齐全且台账内容能正常解析，也不能把
		// 该版本当作有效配方：不能补造物料、不能删除问题版本，也不能改用同一
		// 配方编号下的其他完整版本顶替。该校验针对所有已保存版本，包括尚未
		// 被任何批次引用的版本；台账中另有正常配方与批次，或调用方只查询正常
		// 记录，都不能绕过。必须先于逐条物料的校验判断——没有物料时，物料
		// 编号重复与每份克数规则都无从套用。
		if len(r.Materials) == 0 {
			return fmt.Errorf("%w: 配方编号 %q 版本号 %q 的配方版本没有物料，每个配方版本必须至少包含一种物料",
				ErrCorruptData, r.RecipeNo, r.Version)
		}
		seen := make(map[string]bool, len(r.Materials))
		for _, m := range r.Materials {
			if seen[m.MaterialNo] {
				return fmt.Errorf("%w: 配方 %q 版本 %q 的物料编号 %q 重复",
					ErrCorruptData, r.RecipeNo, r.Version, m.MaterialNo)
			}
			seen[m.MaterialNo] = true
			if m.GramsMilli <= 0 {
				return fmt.Errorf("%w: 配方 %q 版本 %q 物料 %q 的每份克数不是正数",
					ErrCorruptData, r.RecipeNo, r.Version, m.MaterialNo)
			}
		}
	}
	// 再按批次编号唯一标识遍历全部批次记录：同一编号出现第二条记录即数据
	// 损坏，必须先于一切按编号查找的校验拒绝——否则 findBatch 总是返回排在
	// 前面的一条，同一编号查到的配方、投料与数量核对都取决于文件排列顺序。
	// 两条记录即使绑定不同配方版本、计划份数不同或状态不同也属于重复；
	// 内容完全一致同样不是合法记录。不能挑第一条或最后一条、不能合并投料、
	// 也不能自动改号或删除一条后继续。不同批次绑定同一配方版本不受影响。
	batchNos := make(map[string]bool, len(st.Batches))
	for _, b := range st.Batches {
		if b == nil {
			return fmt.Errorf("%w: 台账中存在空的批次记录", ErrCorruptData)
		}
		if batchNos[b.BatchNo] {
			return fmt.Errorf("%w: 批次编号 %q 的批次记录重复", ErrCorruptData, b.BatchNo)
		}
		batchNos[b.BatchNo] = true
	}
	for _, b := range st.Batches {
		// 状态必须精确对应 draft、executing、closed 之一，且必须先于一切
		// 依赖状态语义的校验（草稿带投料、状态机操作、查询视图）判断：
		// 无法识别的状态没有任何既有规则可以套用。缺失、为空、null、写成
		// "ready" 等其他字符串，或大小写不同、前后多了空格，都不能自动
		// 当成某个合法状态——不能按是否已有投料替它猜状态，不能补成草稿，
		// 也不能删除该批次后继续。只要一个批次状态不合法，整份台账即视为
		// 损坏：其他批次与配方完整、本次只想查询别的批次或配方，都不能放行。
		if !isValidBatchStatus(b.Status) {
			if b.Status == "" {
				return fmt.Errorf("%w: 批次 %q 的状态为空或缺失，必须是 %q、%q 或 %q 之一",
					ErrCorruptData, b.BatchNo, StatusDraft, StatusExecuting, StatusClosed)
			}
			return fmt.Errorf("%w: 批次 %q 的状态 %q 无法识别，必须精确为 %q、%q 或 %q 之一（大小写或前后空格不同也不合法）",
				ErrCorruptData, b.BatchNo, b.Status, StatusDraft, StatusExecuting, StatusClosed)
		}
		// 草稿尚未开始执行，不能登记投料；已保存的草稿却带着投料，
		// 说明记录自相矛盾。必须先于配方归属与数量校验拒绝——否则
		// 这份草稿还能被打开、调整份数或改选配方版本，留下按旧计划
		// 登记的投料。不能丢弃投料当作干净草稿，不能自动改状态，也
		// 不能改选版本后继续。
		if b.Status == StatusDraft && len(b.Feedings) > 0 {
			return fmt.Errorf("%w: 批次 %q 仍为草稿状态，却存在 %d 条投料记录",
				ErrCorruptData, b.BatchNo, len(b.Feedings))
		}
		r := findRecipe(st, b.RecipeNo, b.RecipeVersion)
		if r == nil {
			return fmt.Errorf("%w: 批次 %q 绑定的配方 %q 版本 %q 未登记",
				ErrCorruptData, b.BatchNo, b.RecipeNo, b.RecipeVersion)
		}
		if err := validateBatchPlan(b, r); err != nil {
			return err
		}
		if err := validateFeedings(b, r); err != nil {
			return err
		}
	}
	// 最后核对已保存的成功请求：请求记录里保存的返回结果必须是原请求确实
	// 登记成功的那一次结果。配方登记请求与投料请求分别核对，且都必须先于
	// 一切按请求编号重放结果的路径判断——否则重放会把与台账实际内容不符的
	// 保存结果当成第一次成功登记的结果返回。
	if err := validateRecipeRequests(st); err != nil {
		return err
	}
	if err := validateCreateBatchRequests(st); err != nil {
		return err
	}
	if err := validateFeedingRequests(st); err != nil {
		return err
	}
	if err := validateStartBatchRequests(st); err != nil {
		return err
	}
	if err := validateCloseBatchRequests(st); err != nil {
		return err
	}
	return nil
}

// validateBatchPlan 检查一个已保存批次的计划份数与按绑定配方版本算出的
// 应投量。数量规则本身（正份数、千分之一克精度、逐物料上限）统一由
// planRequirements 实现，这里只补充“读取已保存批次”场景的错误分类与
// 批次编号：份数非正或应投量非法都属于 ErrCorruptData。
// 归属以批次实际绑定的版本为准：换用同编号的其他版本或许能算得通，也
// 不能作为接受依据。计划数量是否合法与投料无关：尚未投料、实投不足或
// 超出应投都不影响。
func validateBatchPlan(b *batchRecord, r *recipeRecord) error {
	if b.PlannedPortions <= 0 {
		return fmt.Errorf("%w: 批次 %q 的计划份数 %d 不是正整数",
			ErrCorruptData, b.BatchNo, b.PlannedPortions)
	}
	if _, err := planRequirements(r, b.PlannedPortions); err != nil {
		return fmt.Errorf("%w: 批次 %q 的%v", ErrCorruptData, b.BatchNo, err)
	}
	return nil
}

// validatePlanForRecipe 校验创建或调整草稿批次时，给定份数按配方版本逐
// 物料计算的应投量都能精确表示到千分之一克且不超过上限。份数本身的正
// 整数检查由调用方按各自输入规则完成（调整草稿时份数传 0 表示不改份数，
// 走到这里的必是最终采用的正份数）；数量规则统一由 planRequirements
// 实现，这里透传其逐物料说明（物料、配方编号、版本、份数），由调用方
// 按本次提交包装为 ErrInvalidInput。
func validatePlanForRecipe(r *recipeRecord, portions int) error {
	_, err := planRequirements(r, portions)
	return err
}

// validateFeedings 检查一个批次内已保存投料的登记顺序、合法性与配方归属。
// 归属以批次绑定的配方版本 r 为准：每条投料的物料编号必须在 r 的物料
// 列表中，否则整份台账视为损坏。数量方面只判断数量本身是否合法，不判断
// 实投是否符合配方：无投料的物料累计为零属正常，不足或超过应投量也不
// 在此拒绝。
//
// 序号是整个批次共用的一条登记序列：不同物料的投料依次共用这组序号，
// 不按物料各自另起编号；不同批次则各自从 1 开始。列表中第 i 条（位置
// 从 1 起）的保存序号必须恰好为 i——第一条必须为 1，此后每条加 1。
// 零、负数、漏号、重号、跳号，或只调换了记录位置却没有同步序号，都说明
// 记录已经失去登记先后次序，整份台账视为损坏，即使物料归属与数量全部
// 合法也不能接受。投料时间只由调用方填写、从不参与排序，因此不能用来
// 佐证或否定序号：同一时刻登记、后一条时间早于前一条都属正常。该校验
// 只读取现有序号与位置，绝不重排、补号或删除任何投料。
func validateFeedings(b *batchRecord, r *recipeRecord) error {
	acc := newFeedingAccumulator()
	for i, f := range b.Feedings {
		// 位置 i+1 上的保存序号必须就是 i+1：先于归属与数量校验判断，
		// 因为一旦序号与位置不符，调用方已经无法按序号区分这是哪一次
		// 成功登记——按错误的序号继续核对物料与数量没有意义。不能挑
		// 记录自带的序号当真、不能按投料时间重排，也不能自行补号或
		// 删除后继续。空投料列表不会进入这里，不构成序号错误。
		pos := i + 1
		if f.Seq != pos {
			return fmt.Errorf("%w: 批次 %q 投料列表第 %d 条记录的登记序号为 %d，应为 %d：登记序号必须从 1 起按保存位置逐条连续加 1，不能为零或负数、重号、漏号、跳号或与位置不符",
				ErrCorruptData, b.BatchNo, pos, f.Seq, pos)
		}
		belongs := false
		for _, m := range r.Materials {
			if m.MaterialNo == f.MaterialNo {
				belongs = true
				break
			}
		}
		if !belongs {
			return fmt.Errorf("%w: 批次 %q 的第 %d 条投料物料 %q 不属于其绑定的配方 %q 版本 %q",
				ErrCorruptData, b.BatchNo, f.Seq, f.MaterialNo, b.RecipeNo, b.RecipeVersion)
		}
		if f.GramsMilli <= 0 {
			return fmt.Errorf("%w: 批次 %q 物料 %q 的第 %d 条投料数量不是正数",
				ErrCorruptData, b.BatchNo, f.MaterialNo, f.Seq)
		}
		if !acc.add(f.MaterialNo, f.GramsMilli) {
			return fmt.Errorf("%w: 批次 %q 物料 %q 的累计实投超出上限 %s 克",
				ErrCorruptData, b.BatchNo, f.MaterialNo, maxGramsMilli)
		}
	}
	return nil
}

// validateRecipeRequests 检查台账中每一条已保存的成功配方登记请求：请求记录
// 里保存的返回结果，必须是原请求确实登记成功的那个配方版本——原提交内容、
// 保存的返回结果与台账中的实际登记版本三者必须一致（配方编号、版本号、名称、
// 物料项数、排列顺序、各项物料编号与每份克数）。本次只核对 registerRecipe
// 请求，其他操作的保存结果不在此检查。
//
// 保存结果缺失、为 null、为空对象、无法读成一份完整的配方结果，原请求对应
// 的版本在台账中不存在，或三者内容有任何差异，整份台账即视为损坏：不能仅凭
// 保存结果当作登记成功返回，也不能用同一配方编号下另一个完整版本（R1/v2）
// 顶替原请求登记的版本（R1/v1）——两个版本名称相同或每份用量相同也不算同一
// 版本。核对只读取现有记录，绝不删除请求、补造版本或重新登记。
func validateRecipeRequests(st *persistedState) error {
	for reqNo, req := range st.Requests {
		if req == nil {
			return fmt.Errorf("%w: 请求编号 %q 的请求记录为空", ErrCorruptData, reqNo)
		}
		if req.Op != opRegisterRecipe {
			continue
		}
		if err := validateRecipeRequest(st, reqNo, req); err != nil {
			return err
		}
	}
	return nil
}

// validateRecipeRequest 核对一条已保存的成功配方登记请求。对应关系分两层：
// 保存结果必须与台账中实际登记的版本一致，也必须符合原提交内容（原提交内容
// 在登记成功时就产生了这个版本，两层一致才说明保存结果、实际版本与原请求
// 三者仍是同一次登记）。
//
// 数量按实际克数核对而非字符串写法：原请求填写 1.000、保存结果显示 1 是
// 同一次登记的正常写法差异，0.500 与 0.5 同理，不因末尾零不同误报损坏。
// 这只影响读取核对，不改变请求内容的精确匹配规则——重放判定仍按提交内容
// 原文比较，把 1.000 改成 1 再提交仍是 ErrRequestConflict。
func validateRecipeRequest(st *persistedState, reqNo string, req *requestRecord) error {
	var payload registerRecipePayload
	if err := json.Unmarshal([]byte(req.Payload), &payload); err != nil {
		// 提交内容本身已无法解析时无法确定关联版本，错误信息只指明请求编号。
		return fmt.Errorf("%w: 配方登记请求 %q 保存的提交内容无法解析",
			ErrCorruptData, reqNo)
	}
	// target 用于错误信息：能从原提交内容确定关联版本时一并写明配方编号与版本号。
	target := fmt.Sprintf("配方 %q 版本 %q", payload.RecipeNo, payload.Version)
	if len(req.Result) == 0 {
		return fmt.Errorf("%w: 配方登记请求 %q（%s）缺少保存的登记结果",
			ErrCorruptData, reqNo, target)
	}
	var result RecipeView
	if err := json.Unmarshal(req.Result, &result); err != nil {
		return fmt.Errorf("%w: 配方登记请求 %q（%s）保存的登记结果无法解析为完整配方结果",
			ErrCorruptData, reqNo, target)
	}
	// null、空对象或读不出完整配方（编号、版本、名称、物料任一缺失，物料
	// 为 null/空数组）都不能当作登记成功的结果。
	if result.RecipeNo == "" || result.Version == "" || result.Name == "" || len(result.Materials) == 0 {
		return fmt.Errorf("%w: 配方登记请求 %q（%s）保存的登记结果缺失或不完整",
			ErrCorruptData, reqNo, target)
	}
	// 原请求登记的版本必须确实存在于台账中：版本不存在（即使同编号另有完整
	// 的 R1/v2）时，保存结果不能单独作为登记成功的依据，也不能用其他版本顶替。
	rec := findRecipe(st, payload.RecipeNo, payload.Version)
	if rec == nil {
		return fmt.Errorf("%w: 配方登记请求 %q 对应的 %s 未登记，保存的登记结果不能单独作为登记成功的依据",
			ErrCorruptData, reqNo, target)
	}
	// 编号、版本号、名称：保存结果与实际版本、原提交内容三者必须一致。
	if result.RecipeNo != rec.RecipeNo || result.Version != rec.Version ||
		result.RecipeNo != payload.RecipeNo || result.Version != payload.Version {
		return fmt.Errorf("%w: 配方登记请求 %q（%s）保存结果的配方编号或版本号与原提交内容及实际登记版本不一致",
			ErrCorruptData, reqNo, target)
	}
	if result.Name != rec.Name || result.Name != payload.Name {
		return fmt.Errorf("%w: 配方登记请求 %q（%s）保存结果的名称与原提交内容或实际登记版本不一致",
			ErrCorruptData, reqNo, target)
	}
	// 物料项数必须一致：多一项、少一项都不是同一次登记（含结果为 null 得到
	// 的空列表，已在上面的完整性检查中拒绝）。
	if len(result.Materials) != len(rec.Materials) || len(payload.Materials) != len(rec.Materials) {
		return fmt.Errorf("%w: 配方登记请求 %q（%s）保存结果或原提交内容的物料项数与实际登记版本不一致",
			ErrCorruptData, reqNo, target)
	}
	// 逐项核对排列顺序、物料编号与每份克数。克数换算成千分之一克后精确比较，
	// 原提交 1.000、结果显示 1、实际存 1000 milli 是同一用量。
	for i := range rec.Materials {
		rm := rec.Materials[i]
		vm := result.Materials[i]
		pm := payload.Materials[i]
		if vm.MaterialNo != rm.MaterialNo || pm.MaterialNo != rm.MaterialNo {
			return fmt.Errorf("%w: 配方登记请求 %q（%s）第 %d 项物料编号与实际登记版本不一致",
				ErrCorruptData, reqNo, target, i+1)
		}
		resultMilli, err := parseGrams(vm.Grams)
		if err != nil {
			return fmt.Errorf("%w: 配方登记请求 %q（%s）保存结果物料 %q 的每份克数 %q 不合法",
				ErrCorruptData, reqNo, target, rm.MaterialNo, vm.Grams)
		}
		payloadMilli, err := parseGrams(pm.Grams)
		if err != nil {
			return fmt.Errorf("%w: 配方登记请求 %q（%s）原提交内容物料 %q 的每份克数 %q 不合法",
				ErrCorruptData, reqNo, target, rm.MaterialNo, pm.Grams)
		}
		if resultMilli != rm.GramsMilli || payloadMilli != rm.GramsMilli {
			return fmt.Errorf("%w: 配方登记请求 %q（%s）物料 %q 的每份克数与实际登记版本不一致",
				ErrCorruptData, reqNo, target, rm.MaterialNo)
		}
	}
	return nil
}

// validateCreateBatchRequests 检查台账中每一条已保存的成功创建批次请求：
// 请求记录里保存的返回结果，必须是原请求第一次创建成功时的那份草稿批次
// 结果——原请求所指的批次必须仍然存在，保存结果要对应原提交选定的已登记
// 配方版本（编号、版本号、名称取该版本）与创建时份数，状态为草稿，投料列表
// 为空，各物料按原选版本的顺序完整列出，应投量为每份克数 × 创建时份数、
// 实投量为零、差额为应投量的负值。本次只核对创建批次（createBatch）请求，
// 其他操作的保存结果不在此检查。
//
// 批次后来的草稿调整、开始执行、投料与关闭都属于批次现状，不属于第一次创建
// 的结果：保存结果混入这些内容，或被改成另一批次（即使配方版本、名称、物料
// 与数量完全相同）、同编号配方其他版本的结果，即与首次创建记录不一致。反
// 过来，批次当前已改选配方、调整份数，甚至已执行、投料并关闭，也不能成为
// 拒绝一份合法首次创建结果的理由——核对始终以“创建那一刻”的原提交为准：
// 只要求原批次与原选配方版本仍然存在，不要求批次当前仍是草稿或仍绑定原版本。
//
// 保存结果缺失、为 null、为空对象、无法读成完整批次结果，原提交内容无法
// 解析或不符合创建要求（编号、版本或份数缺失/份数非正），原批次或原选版本
// 不存在，或上述对应关系不成立，整份台账即视为损坏：不能仅凭保存结果当作
// 创建成功返回，不能用另一批次或同编号其他版本顶替，也不能删除请求、补造
// 批次或用当前查询结果覆盖损坏结果。该检查覆盖所有已保存的创建请求，与本次
// 访问哪个批次无关。
func validateCreateBatchRequests(st *persistedState) error {
	for reqNo, req := range st.Requests {
		if req == nil {
			return fmt.Errorf("%w: 请求编号 %q 的请求记录为空", ErrCorruptData, reqNo)
		}
		if req.Op != opCreateBatch {
			continue
		}
		if err := validateCreateBatchRequest(st, reqNo, req); err != nil {
			return err
		}
	}
	return nil
}

// validateCreateBatchRequest 核对一条已保存的成功创建批次请求。对应关系：
// 保存结果必须是原请求（提交内容中的批次编号、配方编号、版本号与份数）第一
// 次创建成功时的视图——批次仍存在于台账中，结果中的批次编号、配方绑定与名称
// （取原提交所选的已登记版本）、计划份数与原提交一致，状态为草稿、投料列表
// 为空，逐物料应投量 = 每份克数 × 创建时份数、实投量为零、差额为应投量的
// 负值。
//
// 核对只认“创建那一刻”的原提交：批次后来调整到别的配方版本或份数、开始
// 执行、追加投料并关闭，都不影响这份历史结果的合法性；不能拿批次当前记录
// 反推创建结果（那会让历史结果被现状覆盖），也不能要求批次当前仍是草稿。
//
// 数量按精确克数核对而非字符串写法：保存结果显示 1、应投量显示 1.000 是同一
// 数量；实投 0 与 0.000、差额 -1 与 -1.000 相同，合法的零实投与负差额不误报。
func validateCreateBatchRequest(st *persistedState, reqNo string, req *requestRecord) error {
	var payload createBatchPayload
	if err := json.Unmarshal([]byte(req.Payload), &payload); err != nil {
		// 提交内容本身已无法解析时无法确定关联批次，错误信息只指明请求编号。
		return fmt.Errorf("%w: 创建批次请求 %q 保存的提交内容无法解析",
			ErrCorruptData, reqNo)
	}
	// 原提交内容必须仍符合创建要求：任一定位字段为空或份数非正，都无法据此
	// 核对“原来的那次创建”。批次编号还能读出来时，错误信息一并写明批次；
	// 批次编号本身缺失时只能指出请求编号。
	if payload.BatchNo == "" {
		return fmt.Errorf("%w: 创建批次请求 %q 保存的提交内容不符合创建要求（批次编号、配方编号、版本号不能为空且份数必须为正整数）",
			ErrCorruptData, reqNo)
	}
	batchNo := payload.BatchNo
	if payload.RecipeNo == "" || payload.Version == "" || payload.Portions <= 0 {
		return fmt.Errorf("%w: 创建批次请求 %q（批次 %q）保存的提交内容不符合创建要求（配方编号、版本号不能为空且份数必须为正整数，得到配方 %q 版本 %q 份数 %d）",
			ErrCorruptData, reqNo, batchNo, payload.RecipeNo, payload.Version, payload.Portions)
	}
	if len(req.Result) == 0 {
		return fmt.Errorf("%w: 创建批次请求 %q（批次 %q）缺少保存的创建结果",
			ErrCorruptData, reqNo, batchNo)
	}
	var result BatchView
	if err := json.Unmarshal(req.Result, &result); err != nil {
		return fmt.Errorf("%w: 创建批次请求 %q（批次 %q）保存的创建结果无法解析为完整批次结果",
			ErrCorruptData, reqNo, batchNo)
	}
	// null、空对象或读不出批次编号的结果都不能当作创建成功的结果。
	if result.BatchNo == "" {
		return fmt.Errorf("%w: 创建批次请求 %q（批次 %q）保存的创建结果缺失或不完整",
			ErrCorruptData, reqNo, batchNo)
	}
	// 原请求对应的批次必须仍然存在：批次没有了，保存的结果不能单独作为创建
	// 成功的依据。批次当前处于什么状态、绑定哪个版本都无关——历史结果只按
	// 原提交核对。
	if findBatch(st, batchNo) == nil {
		return fmt.Errorf("%w: 创建批次请求 %q 对应的批次 %q 不存在，保存的创建结果不能单独作为创建成功的依据",
			ErrCorruptData, reqNo, batchNo)
	}
	// 原提交所选的配方版本必须仍然存在：必须是编号与版本号共同指向的那个
	// 已登记版本，同编号的其他版本不算；名称取该版本的名称。
	r := findRecipe(st, payload.RecipeNo, payload.Version)
	if r == nil {
		return fmt.Errorf("%w: 创建批次请求 %q（批次 %q）原选配方 %q 版本 %q 未登记，同编号其他版本不能顶替",
			ErrCorruptData, reqNo, batchNo, payload.RecipeNo, payload.Version)
	}
	// 保存结果必须指向原请求的批次与原提交的配方绑定、份数：另一批次（即使
	// 配方、名称、物料与数量完全相同）或同编号配方的其他版本不能顶替；批次
	// 后来调整出的新计划也不能回写到首次创建结果里。
	if result.BatchNo != batchNo {
		return fmt.Errorf("%w: 创建批次请求 %q（批次 %q）保存结果的批次编号为 %q，不能用另一批次的结果顶替",
			ErrCorruptData, reqNo, batchNo, result.BatchNo)
	}
	if result.RecipeNo != payload.RecipeNo || result.RecipeVersion != payload.Version ||
		result.RecipeName != r.Name {
		return fmt.Errorf("%w: 创建批次请求 %q（批次 %q）保存结果的配方编号、版本或名称与原提交所选配方 %q 版本 %q 不一致",
			ErrCorruptData, reqNo, batchNo, payload.RecipeNo, payload.Version)
	}
	if result.PlannedPortions != payload.Portions {
		return fmt.Errorf("%w: 创建批次请求 %q（批次 %q）保存结果的计划份数 %d 与原提交份数 %d 不一致",
			ErrCorruptData, reqNo, batchNo, result.PlannedPortions, payload.Portions)
	}
	// 首次创建的结果状态必须是草稿：被改成执行中/已关闭的现状即与首次创建
	// 记录不一致。
	if result.Status != StatusDraft {
		return fmt.Errorf("%w: 创建批次请求 %q（批次 %q）保存结果的状态为 %q，不是草稿",
			ErrCorruptData, reqNo, batchNo, result.Status)
	}
	// 首次创建时不存在任何投料：保存结果里出现投料，说明混入了批次后来的
	// 现状（批次当前可以已投料甚至已关闭，但那不属于创建结果）。
	if len(result.Feedings) != 0 {
		return fmt.Errorf("%w: 创建批次请求 %q（批次 %q）保存结果包含 %d 条投料，首次创建时的投料列表应为空，不能混入后来追加的投料",
			ErrCorruptData, reqNo, batchNo, len(result.Feedings))
	}
	// 逐物料的数量核对必须与首次创建时相符：应投量按原选配方版本与创建时
	// 份数计算，实投量为零，差额为应投量的负值。物料按原选版本的顺序完整
	// 列出，一项不多、一项不少——其他版本独有的物料不能混入，原版本独有
	// 的物料不能丢失。真实的零实投与负差额是合法内容，按精确克数核对即可，
	// 不会误报。
	required, err := planRequirements(r, payload.Portions)
	if err != nil {
		return fmt.Errorf("%w: 创建批次请求 %q（批次 %q）的%v",
			ErrCorruptData, reqNo, batchNo, err)
	}
	if len(result.Materials) != len(required) {
		return fmt.Errorf("%w: 创建批次请求 %q（批次 %q）保存结果的物料核对项数 %d 与原选配方版本的核对项数 %d 不一致",
			ErrCorruptData, reqNo, batchNo, len(result.Materials), len(required))
	}
	for i, item := range required {
		rm := result.Materials[i]
		if rm.MaterialNo != item.materialNo {
			return fmt.Errorf("%w: 创建批次请求 %q（批次 %q）保存结果第 %d 项核对物料 %q 与原选配方版本物料 %q 不一致",
				ErrCorruptData, reqNo, batchNo, i+1, rm.MaterialNo, item.materialNo)
		}
		for _, check := range []struct {
			name string
			got  string
			want gramsMilli
		}{
			{"应投量", rm.RequiredGrams, item.requiredGrams},
			{"实投量", rm.ActualGrams, 0},
			{"差额", rm.DifferenceGrams, -item.requiredGrams},
		} {
			got, err := parseSignedGrams(check.got)
			if err != nil {
				return fmt.Errorf("%w: 创建批次请求 %q（批次 %q）保存结果物料 %q 的%s %q 不合法",
					ErrCorruptData, reqNo, batchNo, item.materialNo, check.name, check.got)
			}
			if got != check.want {
				return fmt.Errorf("%w: 创建批次请求 %q（批次 %q）保存结果物料 %q 的%s %s 与首次创建时的值 %s 不一致",
					ErrCorruptData, reqNo, batchNo, item.materialNo, check.name, check.got, check.want)
			}
		}
	}
	return nil
}

// validateFeedingRequests 检查台账中每一条已保存的成功投料请求：请求记录里
// 保存的返回结果，必须对应原请求批次中确实存在的一条实际投料，且与该序号
// 记录的物料、克数、投料时间、登记人一致，并符合原提交内容。本次只核对
// 投料（addFeeding）请求，其他操作的保存结果不在此检查。
//
// 保存结果缺失、为 null、为空对象，或上述对应关系不成立（批次不存在、
// 序号在该批次中不存在、内容对不上），整份台账即视为损坏：不能仅凭保存
// 结果当作登记成功返回——重放取回的结果必须与批次查询显示的实际投料是同
// 一条。其他批次里同序号的投料不能作为对应记录；物料与数量相同但时间或
// 登记人不同也不算一致。核对只读取现有记录，绝不删除请求、补造投料或
// 重新执行原登记。
func validateFeedingRequests(st *persistedState) error {
	for reqNo, req := range st.Requests {
		if req == nil {
			return fmt.Errorf("%w: 请求编号 %q 的请求记录为空", ErrCorruptData, reqNo)
		}
		if req.Op != opAddFeeding {
			continue
		}
		if err := validateFeedingRequest(st, reqNo, req); err != nil {
			return err
		}
	}
	return nil
}

// validateFeedingRequest 核对一条已保存的成功投料请求。对应关系分两层：
// 保存结果必须与批次中同序号的实际投料一致，也必须符合原提交内容（原
// 提交内容本身在登记时就来自同一次投料，两层一致才说明保存结果、实际
// 投料与原请求三者仍是同一次登记）。
//
// 数量按实际克数核对而非字符串写法：原请求填写 1.000、保存结果显示 1 是
// 同一次登记的正常写法差异；时间按同一时刻核对，不区分时区写法。这些
// 只影响读取核对，不改变请求内容的精确匹配规则——重放判定仍按提交内容
// 原文比较，把 1.000 改成 1 再提交仍是 ErrRequestConflict。
func validateFeedingRequest(st *persistedState, reqNo string, req *requestRecord) error {
	var payload addFeedingPayload
	if err := json.Unmarshal([]byte(req.Payload), &payload); err != nil {
		return fmt.Errorf("%w: 投料请求 %q 保存的提交内容无法解析",
			ErrCorruptData, reqNo)
	}
	batchNo := payload.BatchNo
	if len(req.Result) == 0 {
		return fmt.Errorf("%w: 投料请求 %q（批次 %q）缺少保存的登记结果",
			ErrCorruptData, reqNo, batchNo)
	}
	var result FeedingView
	if err := json.Unmarshal(req.Result, &result); err != nil {
		return fmt.Errorf("%w: 投料请求 %q（批次 %q）保存的登记结果无法解析",
			ErrCorruptData, reqNo, batchNo)
	}
	// 原请求对应的批次必须存在：批次都没有了，保存的结果不能单独作为
	// 登记成功的依据。
	b := findBatch(st, batchNo)
	if b == nil {
		return fmt.Errorf("%w: 投料请求 %q 对应的批次 %q 不存在，保存的登记结果不能单独作为登记成功的依据",
			ErrCorruptData, reqNo, batchNo)
	}
	// 登记序号必须在原请求对应的批次中确实存在：只在该批次的投料列表里
	// 找，其他批次里同序号的投料不能作为对应记录。序号为零或负数、超出
	// 该批次投料条数（含保存结果为 null 或空对象得到的零值）都会在这里
	// 被拒绝。
	var rec *feedingRecord
	for i := range b.Feedings {
		if b.Feedings[i].Seq == result.Seq {
			rec = &b.Feedings[i]
			break
		}
	}
	if rec == nil {
		return fmt.Errorf("%w: 投料请求 %q 保存结果的登记序号 %d 在批次 %q 中不存在",
			ErrCorruptData, reqNo, result.Seq, batchNo)
	}
	// 克数按实际数值核对：保存结果与原提交内容的写法可以不同（1.000 与 1），
	// 但换算成实际克数必须一致；无法解析的克数写法本身就是损坏。
	resultGrams, err := parseGrams(result.Grams)
	if err != nil {
		return fmt.Errorf("%w: 投料请求 %q（批次 %q）保存结果的克数 %q 不合法",
			ErrCorruptData, reqNo, batchNo, result.Grams)
	}
	payloadGrams, err := parseGrams(payload.Grams)
	if err != nil {
		return fmt.Errorf("%w: 投料请求 %q（批次 %q）保存的提交内容克数 %q 不合法",
			ErrCorruptData, reqNo, batchNo, payload.Grams)
	}
	// 保存结果必须与该序号的实际投料一致：物料编号、实际克数、投料时间
	// （同一时刻，不区分时区写法）、登记人逐项核对——不能仅凭物料和数量
	// 相同就忽略时间或登记人的差异。
	if result.MaterialNo != rec.MaterialNo || resultGrams != rec.GramsMilli ||
		!result.Time.Equal(rec.Time) || result.Registrar != rec.Registrar {
		return fmt.Errorf("%w: 投料请求 %q 保存的结果与批次 %q 第 %d 条实际投料不一致（物料、克数、投料时间或登记人不符）",
			ErrCorruptData, reqNo, batchNo, rec.Seq)
	}
	// 保存结果还必须符合原提交内容：同样按物料、实际克数、同一时刻、
	// 登记人核对。
	if result.MaterialNo != payload.MaterialNo || resultGrams != payloadGrams ||
		!result.Time.Equal(payload.Time) || result.Registrar != payload.Registrar {
		return fmt.Errorf("%w: 投料请求 %q 保存的结果与原提交内容（批次 %q）不一致（物料、克数、投料时间或登记人不符）",
			ErrCorruptData, reqNo, batchNo)
	}
	return nil
}

// validateStartBatchRequests 检查台账中每一条已保存的成功开始执行请求：请求
// 记录里保存的返回结果，必须是原请求所指批次第一次开始执行时的那份批次结果——
// 原请求所指的批次必须存在且当前为执行中或已关闭，保存结果要对应这个批次开始
// 时确定的配方编号、版本、名称和计划份数，状态为执行中，投料列表为空，各物料
// 按绑定配方的顺序完整列出，应投量为每份克数 × 计划份数、实投量为零、差额为
// 应投量的负值。本次只核对开始执行（startBatch）请求，其他操作的保存结果不在
// 此检查。
//
// 保存结果缺失、为 null、为空对象、无法读成批次结果，或上述对应关系不成立
// （批次不存在或已退回草稿、批次编号对不上、配方绑定或份数不符、状态不是
// 执行中、混入了后来追加的投料或关闭后的现状、核对项的应投量/实投量/差额
// 对不上），整份台账即视为损坏：不能仅凭保存结果当作开始成功返回，不能用
// 内容相同的另一批次的结果顶替，也不能删除请求、补造批次或用当前查询结果
// 覆盖损坏结果。该检查覆盖所有已保存的开始执行请求，与本次访问哪个批次无关。
func validateStartBatchRequests(st *persistedState) error {
	for reqNo, req := range st.Requests {
		if req == nil {
			return fmt.Errorf("%w: 请求编号 %q 的请求记录为空", ErrCorruptData, reqNo)
		}
		if req.Op != opStartBatch {
			continue
		}
		if err := validateStartBatchRequest(st, reqNo, req); err != nil {
			return err
		}
	}
	return nil
}

// validateStartBatchRequest 核对一条已保存的成功开始执行请求。对应关系：保存
// 结果必须是原请求所指批次（提交内容中的批次编号）第一次开始执行时的视图——
// 批次存在且当前为执行中或已关闭（开始执行只会从草稿前进，不会退回草稿），
// 结果中的批次编号、配方绑定、计划份数与该批次开始时确定的实际记录一致，
// 状态为执行中、投料列表为空、逐物料应投量 = 每份克数 × 计划份数、实投量为
// 零、差额为应投量的负值。
//
// 批次后来的投料与关闭属于批次现状，不属于第一次开始的结果：保存结果里混入
// 这些内容即与首次开始记录不一致；反过来，批次当前已投料或已关闭也不能成为
// 拒绝一份合法首次开始结果的理由——核对始终以“开始那一刻”的内容为准。
//
// 数量按精确克数核对而非字符串写法：保存结果显示 1、应投量显示 1.000 是同一
// 数量；实投 0 与 0.000 相同，合法的负差额不误报。草稿开始前调整过计划的，
// 以批次记录中最终选定的配方版本与份数为准——开始执行后两者已固定，批次
// 记录就是首次开始时的计划。
func validateStartBatchRequest(st *persistedState, reqNo string, req *requestRecord) error {
	var payload startBatchPayload
	if err := json.Unmarshal([]byte(req.Payload), &payload); err != nil {
		// 提交内容本身已无法解析时无法确定关联批次，错误信息只指明请求编号。
		return fmt.Errorf("%w: 开始执行请求 %q 保存的提交内容无法解析",
			ErrCorruptData, reqNo)
	}
	batchNo := payload.BatchNo
	if len(req.Result) == 0 {
		return fmt.Errorf("%w: 开始执行请求 %q（批次 %q）缺少保存的开始结果",
			ErrCorruptData, reqNo, batchNo)
	}
	var result BatchView
	if err := json.Unmarshal(req.Result, &result); err != nil {
		return fmt.Errorf("%w: 开始执行请求 %q（批次 %q）保存的开始结果无法解析为完整批次结果",
			ErrCorruptData, reqNo, batchNo)
	}
	// null、空对象或读不出批次编号的结果都不能当作开始成功的结果。
	if result.BatchNo == "" {
		return fmt.Errorf("%w: 开始执行请求 %q（批次 %q）保存的开始结果缺失或不完整",
			ErrCorruptData, reqNo, batchNo)
	}
	// 原请求所指的批次必须存在：批次没有了，保存的结果不能单独作为开始
	// 成功的依据。
	b := findBatch(st, batchNo)
	if b == nil {
		return fmt.Errorf("%w: 开始执行请求 %q 对应的批次 %q 不存在，保存的开始结果不能单独作为开始成功的依据",
			ErrCorruptData, reqNo, batchNo)
	}
	// 批次当前只能是执行中或已关闭：开始执行后状态只会前进，若被改回草稿，
	// 保存的开始结果与已确认记录不再一致。
	if b.Status != StatusExecuting && b.Status != StatusClosed {
		return fmt.Errorf("%w: 开始执行请求 %q 对应的批次 %q 当前状态为 %s，不是执行中或已关闭，保存的开始结果与已确认记录不一致",
			ErrCorruptData, reqNo, batchNo, b.Status)
	}
	// 保存结果必须指向原请求的批次：内容相同的另一批次的结果不能顶替。
	if result.BatchNo != batchNo {
		return fmt.Errorf("%w: 开始执行请求 %q（批次 %q）保存结果的批次编号为 %q，不能用另一批次的结果顶替",
			ErrCorruptData, reqNo, batchNo, result.BatchNo)
	}
	// 配方绑定与计划份数必须对应批次开始时确定的版本与份数。开始执行后
	// 两者固定，批次当前记录即首次开始时的计划；草稿阶段调整过计划的，
	// 以这份最终选定为准。批次已在前面的校验中确认绑定的配方版本存在，
	// 这里仍防一手。
	r := findRecipe(st, b.RecipeNo, b.RecipeVersion)
	if r == nil {
		return fmt.Errorf("%w: 开始执行请求 %q 对应的批次 %q 绑定的配方 %q 版本 %q 未登记",
			ErrCorruptData, reqNo, batchNo, b.RecipeNo, b.RecipeVersion)
	}
	if result.RecipeNo != b.RecipeNo || result.RecipeVersion != b.RecipeVersion ||
		result.RecipeName != r.Name {
		return fmt.Errorf("%w: 开始执行请求 %q（批次 %q）保存结果的配方编号、版本或名称与批次开始时确定的配方 %q 版本 %q 不一致",
			ErrCorruptData, reqNo, batchNo, b.RecipeNo, b.RecipeVersion)
	}
	if result.PlannedPortions != b.PlannedPortions {
		return fmt.Errorf("%w: 开始执行请求 %q（批次 %q）保存结果的计划份数 %d 与批次开始时确定的份数 %d 不一致",
			ErrCorruptData, reqNo, batchNo, result.PlannedPortions, b.PlannedPortions)
	}
	// 首次开始的结果状态必须是执行中：被改成关闭后的现状或其他值，即与
	// 首次开始记录不一致。
	if result.Status != StatusExecuting {
		return fmt.Errorf("%w: 开始执行请求 %q（批次 %q）保存结果的状态为 %q，不是执行中",
			ErrCorruptData, reqNo, batchNo, result.Status)
	}
	// 首次开始时不存在任何投料：保存结果里出现投料，说明混入了批次后来
	// 的现状，与首次开始记录不一致。
	if len(result.Feedings) != 0 {
		return fmt.Errorf("%w: 开始执行请求 %q（批次 %q）保存结果包含 %d 条投料，首次开始时的投料列表应为空，不能混入后来追加的投料",
			ErrCorruptData, reqNo, batchNo, len(result.Feedings))
	}
	// 逐物料的数量核对必须与首次开始时相符：应投量按批次绑定的配方版本
	// 与计划份数计算，实投量为零，差额为应投量的负值。物料按绑定配方的
	// 顺序完整列出，一项不多、一项不少。真实的零实投与负差额是合法内容，
	// 按精确克数核对即可，不会误报。
	required, err := planRequirements(r, b.PlannedPortions)
	if err != nil {
		return fmt.Errorf("%w: 开始执行请求 %q 对应的批次 %q 的%v",
			ErrCorruptData, reqNo, batchNo, err)
	}
	if len(result.Materials) != len(required) {
		return fmt.Errorf("%w: 开始执行请求 %q（批次 %q）保存结果的物料核对项数 %d 与批次实际核对项数 %d 不一致",
			ErrCorruptData, reqNo, batchNo, len(result.Materials), len(required))
	}
	for i, item := range required {
		rm := result.Materials[i]
		if rm.MaterialNo != item.materialNo {
			return fmt.Errorf("%w: 开始执行请求 %q（批次 %q）保存结果第 %d 项核对物料 %q 与批次实际核对物料 %q 不一致",
				ErrCorruptData, reqNo, batchNo, i+1, rm.MaterialNo, item.materialNo)
		}
		for _, check := range []struct {
			name string
			got  string
			want gramsMilli
		}{
			{"应投量", rm.RequiredGrams, item.requiredGrams},
			{"实投量", rm.ActualGrams, 0},
			{"差额", rm.DifferenceGrams, -item.requiredGrams},
		} {
			got, err := parseSignedGrams(check.got)
			if err != nil {
				return fmt.Errorf("%w: 开始执行请求 %q（批次 %q）保存结果物料 %q 的%s %q 不合法",
					ErrCorruptData, reqNo, batchNo, item.materialNo, check.name, check.got)
			}
			if got != check.want {
				return fmt.Errorf("%w: 开始执行请求 %q（批次 %q）保存结果物料 %q 的%s %s 与首次开始时的值 %s 不一致",
					ErrCorruptData, reqNo, batchNo, item.materialNo, check.name, check.got, check.want)
			}
		}
	}
	return nil
}

// validateCloseBatchRequests 检查台账中每一条已保存的成功关闭请求：请求记录里
// 保存的返回结果，必须是原请求所指批次第一次关闭时确认的那份完整批次结果——
// 原请求所指的批次必须存在并仍为已关闭，保存结果要对应这个批次实际绑定的配方
// 编号、版本、名称和计划份数，保留全部投料及其登记顺序，逐物料的应投量、实投
// 量和差额也要与该批次相符。本次只核对关闭（closeBatch）请求，其他操作的保存
// 结果不在此检查。
//
// 保存结果缺失、为 null、为空对象、无法读成批次结果，或上述对应关系不成立
// （批次不存在或不再是已关闭、批次编号对不上、配方绑定或份数不符、投料少一条/
// 多一条/被合并/顺序或内容有差异、核对项的应投量/实投量/差额对不上），整份台账
// 即视为损坏：不能仅凭保存结果当作关闭成功返回，不能用另一批次的结果顶替
// （哪怕配方与投料数量相同），也不能删除请求、重做关闭或用当前查询结果覆盖
// 损坏结果。该检查覆盖所有已保存的关闭请求，与本次访问哪个批次无关。
func validateCloseBatchRequests(st *persistedState) error {
	for reqNo, req := range st.Requests {
		if req == nil {
			return fmt.Errorf("%w: 请求编号 %q 的请求记录为空", ErrCorruptData, reqNo)
		}
		if req.Op != opCloseBatch {
			continue
		}
		if err := validateCloseBatchRequest(st, reqNo, req); err != nil {
			return err
		}
	}
	return nil
}

// validateCloseBatchRequest 核对一条已保存的成功关闭请求。对应关系：保存结果
// 必须是原请求所指批次（提交内容中的批次编号）当前已关闭状态的完整视图——
// 批次存在且仍为已关闭，结果中的批次编号、配方绑定、计划份数、状态、全部
// 投料（登记顺序）与逐物料数量核对都与该批次的实际记录一致。
//
// 数量按精确克数核对而非字符串写法：保存结果显示 1、批次记录为 1.000 是同一
// 数量的正常写法差异；时间按同一时刻核对，不区分时区写法。关闭只确认已有
// 投料、不要求数量吻合，因此没有投料、实投为零或差额为负都是合法内容，
// 不能当成损坏。
func validateCloseBatchRequest(st *persistedState, reqNo string, req *requestRecord) error {
	var payload closeBatchPayload
	if err := json.Unmarshal([]byte(req.Payload), &payload); err != nil {
		// 提交内容本身已无法解析时无法确定关联批次，错误信息只指明请求编号。
		return fmt.Errorf("%w: 关闭请求 %q 保存的提交内容无法解析",
			ErrCorruptData, reqNo)
	}
	batchNo := payload.BatchNo
	if len(req.Result) == 0 {
		return fmt.Errorf("%w: 关闭请求 %q（批次 %q）缺少保存的关闭结果",
			ErrCorruptData, reqNo, batchNo)
	}
	var result BatchView
	if err := json.Unmarshal(req.Result, &result); err != nil {
		return fmt.Errorf("%w: 关闭请求 %q（批次 %q）保存的关闭结果无法解析为完整批次结果",
			ErrCorruptData, reqNo, batchNo)
	}
	// null、空对象或读不出批次编号的结果都不能当作关闭成功的结果。
	if result.BatchNo == "" {
		return fmt.Errorf("%w: 关闭请求 %q（批次 %q）保存的关闭结果缺失或不完整",
			ErrCorruptData, reqNo, batchNo)
	}
	// 原请求所指的批次必须存在并仍为已关闭：批次没有了，或已被改回其他
	// 状态，保存的结果都不能单独作为关闭成功的依据。
	b := findBatch(st, batchNo)
	if b == nil {
		return fmt.Errorf("%w: 关闭请求 %q 对应的批次 %q 不存在，保存的关闭结果不能单独作为关闭成功的依据",
			ErrCorruptData, reqNo, batchNo)
	}
	if b.Status != StatusClosed {
		return fmt.Errorf("%w: 关闭请求 %q 对应的批次 %q 当前状态为 %s，不是已关闭，保存的关闭结果与已确认记录不一致",
			ErrCorruptData, reqNo, batchNo, b.Status)
	}
	// 保存结果必须指向原请求的批次：另一批次的结果即使配方与投料数量
	// 完全相同，也不能顶替。
	if result.BatchNo != batchNo {
		return fmt.Errorf("%w: 关闭请求 %q（批次 %q）保存结果的批次编号为 %q，不能用另一批次的结果顶替",
			ErrCorruptData, reqNo, batchNo, result.BatchNo)
	}
	// 配方绑定与计划份数必须对应该批次实际绑定的版本与份数。批次已在
	// 前面的校验中确认绑定的配方版本存在，这里仍防一手。
	r := findRecipe(st, b.RecipeNo, b.RecipeVersion)
	if r == nil {
		return fmt.Errorf("%w: 关闭请求 %q 对应的批次 %q 绑定的配方 %q 版本 %q 未登记",
			ErrCorruptData, reqNo, batchNo, b.RecipeNo, b.RecipeVersion)
	}
	if result.RecipeNo != b.RecipeNo || result.RecipeVersion != b.RecipeVersion ||
		result.RecipeName != r.Name {
		return fmt.Errorf("%w: 关闭请求 %q（批次 %q）保存结果的配方编号、版本或名称与批次实际绑定的配方 %q 版本 %q 不一致",
			ErrCorruptData, reqNo, batchNo, b.RecipeNo, b.RecipeVersion)
	}
	if result.PlannedPortions != b.PlannedPortions {
		return fmt.Errorf("%w: 关闭请求 %q（批次 %q）保存结果的计划份数 %d 与批次实际份数 %d 不一致",
			ErrCorruptData, reqNo, batchNo, result.PlannedPortions, b.PlannedPortions)
	}
	// 关闭结果确认的就是已关闭状态：状态被改成其他值即与已确认记录不一致。
	if result.Status != StatusClosed {
		return fmt.Errorf("%w: 关闭请求 %q（批次 %q）保存结果的状态为 %q，不是已关闭",
			ErrCorruptData, reqNo, batchNo, result.Status)
	}
	// 投料必须一条不多、一条不少地按登记顺序保留：同物料的多次投料不能
	// 合并，序号、物料、数量、时间与登记人逐项核对。数量按精确克数比较
	// （1.000 与 1 是同一数量），时间按同一时刻比较（不区分时区写法）。
	if len(result.Feedings) != len(b.Feedings) {
		return fmt.Errorf("%w: 关闭请求 %q（批次 %q）保存结果的投料条数 %d 与批次实际投料条数 %d 不一致，不能少一条、多一条或合并同物料的记录",
			ErrCorruptData, reqNo, batchNo, len(result.Feedings), len(b.Feedings))
	}
	for i := range b.Feedings {
		rec := b.Feedings[i]
		fv := result.Feedings[i]
		fg, err := parseGrams(fv.Grams)
		if err != nil {
			return fmt.Errorf("%w: 关闭请求 %q（批次 %q）保存结果第 %d 条投料的克数 %q 不合法",
				ErrCorruptData, reqNo, batchNo, i+1, fv.Grams)
		}
		if fv.Seq != rec.Seq || fv.MaterialNo != rec.MaterialNo || fg != rec.GramsMilli ||
			!fv.Time.Equal(rec.Time) || fv.Registrar != rec.Registrar {
			return fmt.Errorf("%w: 关闭请求 %q（批次 %q）保存结果第 %d 条投料与批次实际投料不一致（序号、物料、克数、投料时间或登记人不符）",
				ErrCorruptData, reqNo, batchNo, i+1)
		}
	}
	// 逐物料的数量核对也必须与该批次相符：应投量按批次实际绑定的配方
	// 版本与计划份数计算，实投量按实际投料累计，差额为实投减应投。只改
	// 坏核对差额、原始投料仍完整，同样属于结果与已确认记录不一致。真实
	// 的零实投与负差额是合法内容，按精确克数核对即可，不会误报。
	required, err := planRequirements(r, b.PlannedPortions)
	if err != nil {
		return fmt.Errorf("%w: 关闭请求 %q 对应的批次 %q 的%v",
			ErrCorruptData, reqNo, batchNo, err)
	}
	acc := newFeedingAccumulator()
	for _, f := range b.Feedings {
		if !acc.add(f.MaterialNo, f.GramsMilli) {
			return fmt.Errorf("%w: 关闭请求 %q 对应的批次 %q 物料 %q 的累计实投超出可表示范围",
				ErrCorruptData, reqNo, batchNo, f.MaterialNo)
		}
	}
	if len(result.Materials) != len(required) {
		return fmt.Errorf("%w: 关闭请求 %q（批次 %q）保存结果的物料核对项数 %d 与批次实际核对项数 %d 不一致",
			ErrCorruptData, reqNo, batchNo, len(result.Materials), len(required))
	}
	for i, item := range required {
		rm := result.Materials[i]
		if rm.MaterialNo != item.materialNo {
			return fmt.Errorf("%w: 关闭请求 %q（批次 %q）保存结果第 %d 项核对物料 %q 与批次实际核对物料 %q 不一致",
				ErrCorruptData, reqNo, batchNo, i+1, rm.MaterialNo, item.materialNo)
		}
		actual := acc.total(item.materialNo)
		diff := actual - item.requiredGrams
		for _, check := range []struct {
			name string
			got  string
			want gramsMilli
		}{
			{"应投量", rm.RequiredGrams, item.requiredGrams},
			{"实投量", rm.ActualGrams, actual},
			{"差额", rm.DifferenceGrams, diff},
		} {
			got, err := parseSignedGrams(check.got)
			if err != nil {
				return fmt.Errorf("%w: 关闭请求 %q（批次 %q）保存结果物料 %q 的%s %q 不合法",
					ErrCorruptData, reqNo, batchNo, item.materialNo, check.name, check.got)
			}
			if got != check.want {
				return fmt.Errorf("%w: 关闭请求 %q（批次 %q）保存结果物料 %q 的%s %s 与批次实际值 %s 不一致",
					ErrCorruptData, reqNo, batchNo, item.materialNo, check.name, check.got, check.want)
			}
		}
	}
	return nil
}

// persist 先写临时文件再原子改名，并 fsync 目录，保证已提交数据可恢复。
func (s *Store) persist(st *persistedState) error {
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	tmpPath := filepath.Join(s.dir, stateFileName+".tmp")
	f, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, filepath.Join(s.dir, stateFileName)); err != nil {
		return err
	}
	if d, err := os.Open(s.dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

func cloneState(st *persistedState) (*persistedState, error) {
	data, err := json.Marshal(st)
	if err != nil {
		return nil, err
	}
	var out persistedState
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, err
	}
	if out.Requests == nil {
		out.Requests = map[string]*requestRecord{}
	}
	return &out, nil
}

// write 是所有写入操作的统一入口：
//   - reqNo 为空直接报错；
//   - 同一 reqNo 且操作与内容完全相同，返回第一次成功的结果（幂等重放）；
//   - 同一 reqNo 用于其他操作或不同内容，返回 ErrRequestConflict；
//   - apply 在状态副本上执行，返回校验错误时不落盘、不占用请求编号；
//   - apply 成功后，状态与请求记录一起原子落盘，再切换到内存状态。
func (s *Store) write(reqNo, op string, payload any, apply func(*persistedState) (json.RawMessage, error), out any) error {
	if reqNo == "" {
		return errors.New("请求编号不能为空")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.lockExclusive(); err != nil {
		return err
	}
	defer s.unlock()

	// 跨进程场景下，其他进程可能已提交新数据；加锁后重新加载，
	// 确保在最新状态上应用变更，避免用旧快照覆盖其他进程的写入。
	if err := s.load(); err != nil {
		return err
	}

	payloadRaw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	payloadStr := string(payloadRaw)

	if req, ok := s.state.Requests[reqNo]; ok {
		if req.Op != op || req.Payload != payloadStr {
			return fmt.Errorf("%w: 请求编号 %q 已用于操作 %q，不能再用于操作 %q 或不同内容",
				ErrRequestConflict, reqNo, req.Op, op)
		}
		// 保存的配方登记结果、创建批次结果、投料结果、开始执行结果与关闭结果
		// 都已在本次 load 的 validateState 中与实际登记内容核对一致（缺失、为
		// null、为空对象或内容不符都已在上面拒绝），这里取回的就是第一次成功
		// 的结果。
		if err := json.Unmarshal(req.Result, out); err != nil {
			return fmt.Errorf("重放请求 %q 的结果失败: %w", reqNo, err)
		}
		return nil
	}

	clone, err := cloneState(s.state)
	if err != nil {
		return err
	}
	result, err := apply(clone)
	if err != nil {
		return err
	}
	clone.Requests[reqNo] = &requestRecord{Op: op, Payload: payloadStr, Result: result}
	if err := s.persist(clone); err != nil {
		return fmt.Errorf("保存台账失败: %w", err)
	}
	s.state = clone
	if err := json.Unmarshal(result, out); err != nil {
		return fmt.Errorf("解析请求结果失败: %w", err)
	}
	return nil
}

// read 在共享锁下读取当前状态。
func (s *Store) read(fn func(*persistedState) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.lockShared(); err != nil {
		return err
	}
	defer s.unlock()
	if err := s.load(); err != nil {
		return err
	}
	return fn(s.state)
}

// 视图构造：每次都从记录全新构造，返回的切片与内部状态完全隔离。

func buildRecipeView(r *recipeRecord) *RecipeView {
	mats := make([]MaterialView, 0, len(r.Materials))
	for _, m := range r.Materials {
		mats = append(mats, MaterialView{MaterialNo: m.MaterialNo, Grams: m.GramsMilli.String()})
	}
	return &RecipeView{RecipeNo: r.RecipeNo, Version: r.Version, Name: r.Name, Materials: mats}
}

func buildBatchView(b *batchRecord, r *recipeRecord) (*BatchView, error) {
	v := &BatchView{
		BatchNo:         b.BatchNo,
		RecipeNo:        r.RecipeNo,
		RecipeVersion:   r.Version,
		RecipeName:      r.Name,
		PlannedPortions: b.PlannedPortions,
		Status:          b.Status,
		Feedings:        make([]FeedingView, 0, len(b.Feedings)),
		Materials:       make([]MaterialRequirement, 0, len(r.Materials)),
	}

	for _, f := range b.Feedings {
		v.Feedings = append(v.Feedings, FeedingView{
			Seq:        f.Seq,
			MaterialNo: f.MaterialNo,
			Grams:      f.GramsMilli.String(),
			Time:       f.Time,
			Registrar:  f.Registrar,
		})
	}

	// 按物料累计实投量；台账在读取时已通过 validateFeedings 校验，
	// 这里仍用同一套累计规则防一手整数回绕。
	acc := newFeedingAccumulator()
	for _, f := range b.Feedings {
		if !acc.add(f.MaterialNo, f.GramsMilli) {
			return nil, fmt.Errorf("%w: 批次 %q 物料 %q 的累计实投超出可表示范围",
				ErrCorruptData, b.BatchNo, f.MaterialNo)
		}
	}

	// 数量核对按配方物料逐项列出；应投量统一由 planRequirements 按本批次
	// 绑定版本与计划份数计算，与创建、调整、读取校验共用同一套规则。
	// 台账在读取时已通过 validateState 校验，这里仍防一手同一版本内物料
	// 编号重复，避免返回重复的数量核对项。
	required, err := planRequirements(r, b.PlannedPortions)
	if err != nil {
		return nil, fmt.Errorf("%w: 批次 %q 的%v", ErrCorruptData, b.BatchNo, err)
	}
	seen := make(map[string]bool, len(required))
	for _, item := range required {
		if seen[item.materialNo] {
			return nil, fmt.Errorf("%w: 配方 %q 版本 %q 的物料编号 %q 重复",
				ErrCorruptData, r.RecipeNo, r.Version, item.materialNo)
		}
		seen[item.materialNo] = true
		act := acc.total(item.materialNo)
		v.Materials = append(v.Materials, MaterialRequirement{
			MaterialNo:      item.materialNo,
			RequiredGrams:   item.requiredGrams.String(),
			ActualGrams:     act.String(),
			DifferenceGrams: (act - item.requiredGrams).String(),
		})
	}
	return v, nil
}

// recipeKey 是配方的唯一标识：配方编号与版本号的组合，采用精确匹配。
type recipeKey struct {
	recipeNo string
	version  string
}

func findRecipe(st *persistedState, recipeNo, version string) *recipeRecord {
	for _, r := range st.Recipes {
		if r.RecipeNo == recipeNo && r.Version == version {
			return r
		}
	}
	return nil
}

func findBatch(st *persistedState, batchNo string) *batchRecord {
	for _, b := range st.Batches {
		if b.BatchNo == batchNo {
			return b
		}
	}
	return nil
}
