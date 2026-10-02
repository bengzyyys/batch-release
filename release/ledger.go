package release

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// 台账错误的具体原因，调用方可用 errors.Is 判断。
var (
	ErrNotFound          = errors.New("release: 批次未找到")
	ErrRecipeNotFound    = errors.New("release: 配方版本不存在")
	ErrRecipeExists      = errors.New("release: 配方版本已登记，内容不可覆盖")
	ErrBatchExists       = errors.New("release: 批次编号重复")
	ErrInvalidState      = errors.New("release: 操作不符合批次当前状态")
	ErrInvalidQuantity   = errors.New("release: 数量不合法")
	ErrInvalidInput      = errors.New("release: 输入内容不合法")
	ErrDuplicateMaterial = errors.New("release: 同一版本内物料编号重复")
	ErrUnknownMaterial   = errors.New("release: 物料不属于该批次绑定的配方版本")
	ErrRequestConflict   = errors.New("release: 请求编号已用于另一操作或不同内容")
	ErrCorruptData       = errors.New("release: 已有数据无法读取")
)

// BatchStatus 是批次的生命周期状态。
type BatchStatus string

const (
	StatusDraft   BatchStatus = "draft"   // 草稿：可调整份数或改选配方版本
	StatusRunning BatchStatus = "running" // 执行中：份数与配方固定，可追加投料，可关闭
	StatusClosed  BatchStatus = "closed"  // 已关闭：确认已有投料，不能再变更
)

// Grams 以毫克为单位的克数，内部用整数保存，保证数量核对精确。
// 正数且最多三位小数的克数才能构造成功（见 ParseGrams）。
type Grams int64

var gramsPattern = regexp.MustCompile(`^[0-9]+(\.[0-9]{1,3})?$`)

// ParseGrams 解析十进制克数字符串，要求为正数且最多三位小数。
func ParseGrams(s string) (Grams, error) {
	if !gramsPattern.MatchString(s) {
		return 0, fmt.Errorf("%w: 克数 %q 需为正数且最多三位小数", ErrInvalidQuantity, s)
	}
	whole, frac, _ := strings.Cut(s, ".")
	wholeN, err := strconv.ParseInt(whole, 10, 64)
	if err != nil || wholeN > (1<<62)/1000 {
		return 0, fmt.Errorf("%w: 克数 %q 超出可表示范围", ErrInvalidQuantity, s)
	}
	mg := wholeN * 1000
	for i, scale := int64(0), int64(100); i < int64(len(frac)); i, scale = i+1, scale/10 {
		mg += int64(frac[i]-'0') * scale
	}
	if mg <= 0 {
		return 0, fmt.Errorf("%w: 克数必须为正数", ErrInvalidQuantity)
	}
	return Grams(mg), nil
}

// String 以克为单位格式化，去掉多余的尾零。
func (g Grams) String() string {
	mg := int64(g)
	if mg%1000 == 0 {
		return strconv.FormatInt(mg/1000, 10)
	}
	frac := fmt.Sprintf("%03d", mg%1000)
	frac = strings.TrimRight(frac, "0")
	return strconv.FormatInt(mg/1000, 10) + "." + frac
}

// Material 是配方中的一种物料：物料编号和生产一份需要的克数。
type Material struct {
	MaterialID string
	Grams      Grams
}

// RecipeVersion 是由配方编号和版本号共同识别的一个配方版本。
type RecipeVersion struct {
	RecipeID  string
	Version   string
	Name      string
	Materials []Material
}

// Feeding 是一条已登记的投料记录。
type Feeding struct {
	MaterialID string
	Grams      Grams
	FedAt      time.Time
	Operator   string
}

// MaterialReport 是查询批次时单个物料的数量核对。
type MaterialReport struct {
	MaterialID string
	Required   Grams // 应投量 = 单份克数 × 计划份数
	Actual     Grams // 累计实投量
	Diff       Grams // 实投减应投
}

// BatchReport 是按批次查询返回的完整记录。
type BatchReport struct {
	BatchID    string
	RecipeID   string
	Version    string
	RecipeName string
	Units      int
	Status     BatchStatus
	Feedings   []Feeding
	Materials  []MaterialReport
}

// 持久化到磁盘的台账数据。
type diskState struct {
	Recipes  map[string]RecipeVersion `json:"recipes"`  // 键为 配方编号+"\x00"+版本号
	Batches  map[string]*batchState   `json:"batches"`  // 键为批次编号
	Requests map[string]string        `json:"requests"` // 请求编号 -> 首次成功的操作内容指纹
}

type batchState struct {
	BatchID  string
	RecipeID string
	Version  string
	Units    int
	Status   BatchStatus
	Feedings []Feeding // 按成功登记的先后顺序
}

// Ledger 是一本本地台账。不同数据位置的 Ledger 互相独立。
type Ledger struct {
	mu   sync.Mutex
	path string
	data diskState
}

const ledgerFileName = "ledger.json"

func recipeKey(recipeID, version string) string { return recipeID + "\x00" + version }

// Open 打开 dir 位置上的台账。空位置得到空台账；位置下已有数据但
// 无法读取或解析时返回 ErrCorruptData，绝不当作空台账继续保存。
func Open(dir string) (*Ledger, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("release: 无法使用数据位置 %s: %w", dir, err)
	}
	l := &Ledger{
		path: filepath.Join(dir, ledgerFileName),
		data: diskState{
			Recipes:  map[string]RecipeVersion{},
			Batches:  map[string]*batchState{},
			Requests: map[string]string{},
		},
	}
	raw, err := os.ReadFile(l.path)
	if errors.Is(err, os.ErrNotExist) {
		return l, nil
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCorruptData, err)
	}
	if err := json.Unmarshal(raw, &l.data); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCorruptData, err)
	}
	if l.data.Recipes == nil {
		l.data.Recipes = map[string]RecipeVersion{}
	}
	if l.data.Batches == nil {
		l.data.Batches = map[string]*batchState{}
	}
	if l.data.Requests == nil {
		l.data.Requests = map[string]string{}
	}
	return l, nil
}

// persist 把台账原子写回磁盘（先写临时文件再改名），调用前须持有锁。
func (l *Ledger) persist() error {
	raw, err := json.MarshalIndent(l.data, "", "  ")
	if err != nil {
		return err
	}
	tmp := l.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return fmt.Errorf("release: 保存台账失败: %w", err)
	}
	if err := os.Rename(tmp, l.path); err != nil {
		return fmt.Errorf("release: 保存台账失败: %w", err)
	}
	return nil
}

// begin 处理请求编号幂等。返回 replay=true 表示同一编号相同内容曾成功过，
// 调用方应直接返回首次成功的结果（nil），不再产生业务变更。
func (l *Ledger) begin(reqID, fingerprint string) (replay bool, err error) {
	if reqID == "" {
		return false, fmt.Errorf("%w: 请求编号不能为空", ErrInvalidInput)
	}
	if prev, ok := l.data.Requests[reqID]; ok {
		if prev != fingerprint {
			return false, fmt.Errorf("%w: %q", ErrRequestConflict, reqID)
		}
		return true, nil
	}
	return false, nil
}

// commit 在业务变更成功后登记请求编号并持久化，调用前须持有锁。
func (l *Ledger) commit(reqID, fingerprint string) error {
	l.data.Requests[reqID] = fingerprint
	return l.persist()
}

// RegisterRecipe 登记一个配方版本。同一配方编号和版本号已登记时返回
// ErrRecipeExists，已登记的内容不可覆盖。
func (l *Ledger) RegisterRecipe(reqID string, rv RecipeVersion) error {
	fp := fingerprintOf("register-recipe", rv.RecipeID, rv.Version, rv.Name, rv.Materials)
	l.mu.Lock()
	defer l.mu.Unlock()
	if replay, err := l.begin(reqID, fp); replay || err != nil {
		return err
	}
	if rv.RecipeID == "" || rv.Version == "" || rv.Name == "" {
		return fmt.Errorf("%w: 配方编号、版本号和名称不能为空", ErrInvalidInput)
	}
	if len(rv.Materials) == 0 {
		return fmt.Errorf("%w: 配方版本至少包含一种物料", ErrInvalidInput)
	}
	seen := make(map[string]bool, len(rv.Materials))
	for _, m := range rv.Materials {
		if m.MaterialID == "" {
			return fmt.Errorf("%w: 物料编号不能为空", ErrInvalidInput)
		}
		if seen[m.MaterialID] {
			return fmt.Errorf("%w: %q", ErrDuplicateMaterial, m.MaterialID)
		}
		seen[m.MaterialID] = true
		if m.Grams <= 0 {
			return fmt.Errorf("%w: 物料 %q 的克数必须为正数且最多三位小数", ErrInvalidQuantity, m.MaterialID)
		}
	}
	key := recipeKey(rv.RecipeID, rv.Version)
	if _, ok := l.data.Recipes[key]; ok {
		return fmt.Errorf("%w: %s 版本 %s", ErrRecipeExists, rv.RecipeID, rv.Version)
	}
	mats := make([]Material, len(rv.Materials))
	copy(mats, rv.Materials)
	l.data.Recipes[key] = RecipeVersion{
		RecipeID:  rv.RecipeID,
		Version:   rv.Version,
		Name:      rv.Name,
		Materials: mats,
	}
	return l.commit(reqID, fp)
}

// GetRecipe 查询已登记的配方版本，返回的是副本。
func (l *Ledger) GetRecipe(recipeID, version string) (RecipeVersion, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	rv, ok := l.data.Recipes[recipeKey(recipeID, version)]
	if !ok {
		return RecipeVersion{}, fmt.Errorf("%w: %s 版本 %s", ErrRecipeNotFound, recipeID, version)
	}
	mats := make([]Material, len(rv.Materials))
	copy(mats, rv.Materials)
	rv.Materials = mats
	return rv, nil
}

// CreateBatch 用独立批次编号建立草稿批次，绑定已登记的配方版本和计划份数。
func (l *Ledger) CreateBatch(reqID, batchID, recipeID, version string, units int) error {
	fp := fingerprintOf("create-batch", batchID, recipeID, version, units)
	l.mu.Lock()
	defer l.mu.Unlock()
	if replay, err := l.begin(reqID, fp); replay || err != nil {
		return err
	}
	if batchID == "" {
		return fmt.Errorf("%w: 批次编号不能为空", ErrInvalidInput)
	}
	if _, ok := l.data.Batches[batchID]; ok {
		return fmt.Errorf("%w: %q", ErrBatchExists, batchID)
	}
	if _, ok := l.data.Recipes[recipeKey(recipeID, version)]; !ok {
		return fmt.Errorf("%w: %s 版本 %s", ErrRecipeNotFound, recipeID, version)
	}
	if units <= 0 {
		return fmt.Errorf("%w: 计划份数必须为正整数，得到 %d", ErrInvalidQuantity, units)
	}
	l.data.Batches[batchID] = &batchState{
		BatchID:  batchID,
		RecipeID: recipeID,
		Version:  version,
		Units:    units,
		Status:   StatusDraft,
	}
	return l.commit(reqID, fp)
}

// UpdateDraft 调整草稿批次的计划份数或改选配方版本；开始执行后两者固定。
func (l *Ledger) UpdateDraft(reqID, batchID, recipeID, version string, units int) error {
	fp := fingerprintOf("update-draft", batchID, recipeID, version, units)
	l.mu.Lock()
	defer l.mu.Unlock()
	if replay, err := l.begin(reqID, fp); replay || err != nil {
		return err
	}
	b, ok := l.data.Batches[batchID]
	if !ok {
		return fmt.Errorf("%w: %q", ErrNotFound, batchID)
	}
	if b.Status != StatusDraft {
		return fmt.Errorf("%w: 批次 %q 已开始执行，计划份数和配方版本已固定", ErrInvalidState, batchID)
	}
	if _, ok := l.data.Recipes[recipeKey(recipeID, version)]; !ok {
		return fmt.Errorf("%w: %s 版本 %s", ErrRecipeNotFound, recipeID, version)
	}
	if units <= 0 {
		return fmt.Errorf("%w: 计划份数必须为正整数，得到 %d", ErrInvalidQuantity, units)
	}
	b.RecipeID, b.Version, b.Units = recipeID, version, units
	return l.commit(reqID, fp)
}

// StartBatch 把草稿批次转为执行中。
func (l *Ledger) StartBatch(reqID, batchID string) error {
	fp := fingerprintOf("start-batch", batchID)
	l.mu.Lock()
	defer l.mu.Unlock()
	if replay, err := l.begin(reqID, fp); replay || err != nil {
		return err
	}
	b, ok := l.data.Batches[batchID]
	if !ok {
		return fmt.Errorf("%w: %q", ErrNotFound, batchID)
	}
	if b.Status != StatusDraft {
		return fmt.Errorf("%w: 批次 %q 当前为 %s，只有草稿可以开始执行", ErrInvalidState, batchID, b.Status)
	}
	b.Status = StatusRunning
	return l.commit(reqID, fp)
}

// AddFeeding 给执行中的批次追加一条投料。允许实际投料不足或超过应投量；
// 已登记的投料不能修改或删除。
func (l *Ledger) AddFeeding(reqID, batchID, materialID string, grams Grams, fedAt time.Time, operator string) error {
	fp := fingerprintOf("add-feeding", batchID, materialID, grams, fedAt.UTC(), operator)
	l.mu.Lock()
	defer l.mu.Unlock()
	if replay, err := l.begin(reqID, fp); replay || err != nil {
		return err
	}
	b, ok := l.data.Batches[batchID]
	if !ok {
		return fmt.Errorf("%w: %q", ErrNotFound, batchID)
	}
	if b.Status != StatusRunning {
		return fmt.Errorf("%w: 批次 %q 当前为 %s，只有执行中的批次可以追加投料", ErrInvalidState, batchID, b.Status)
	}
	if grams <= 0 {
		return fmt.Errorf("%w: 投料克数必须为正数且最多三位小数", ErrInvalidQuantity)
	}
	rv := l.data.Recipes[recipeKey(b.RecipeID, b.Version)]
	found := false
	for _, m := range rv.Materials {
		if m.MaterialID == materialID {
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("%w: %q 不在配方 %s 版本 %s 中", ErrUnknownMaterial, materialID, b.RecipeID, b.Version)
	}
	b.Feedings = append(b.Feedings, Feeding{
		MaterialID: materialID,
		Grams:      grams,
		FedAt:      fedAt,
		Operator:   operator,
	})
	return l.commit(reqID, fp)
}

// CloseBatch 关闭执行中的批次，表示确认已有投料，不要求数量已经吻合。
// 关闭后不能追加投料，也不能重新打开。
func (l *Ledger) CloseBatch(reqID, batchID string) error {
	fp := fingerprintOf("close-batch", batchID)
	l.mu.Lock()
	defer l.mu.Unlock()
	if replay, err := l.begin(reqID, fp); replay || err != nil {
		return err
	}
	b, ok := l.data.Batches[batchID]
	if !ok {
		return fmt.Errorf("%w: %q", ErrNotFound, batchID)
	}
	if b.Status != StatusRunning {
		return fmt.Errorf("%w: 批次 %q 当前为 %s，只有执行中的批次可以关闭", ErrInvalidState, batchID, b.Status)
	}
	b.Status = StatusClosed
	return l.commit(reqID, fp)
}

// GetBatch 按批次查询完整记录。返回的是副本，调用方修改不影响台账。
func (l *Ledger) GetBatch(batchID string) (BatchReport, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	b, ok := l.data.Batches[batchID]
	if !ok {
		return BatchReport{}, fmt.Errorf("%w: %q", ErrNotFound, batchID)
	}
	rv := l.data.Recipes[recipeKey(b.RecipeID, b.Version)]
	rep := BatchReport{
		BatchID:    b.BatchID,
		RecipeID:   b.RecipeID,
		Version:    b.Version,
		RecipeName: rv.Name,
		Units:      b.Units,
		Status:     b.Status,
		Feedings:   make([]Feeding, len(b.Feedings)),
	}
	copy(rep.Feedings, b.Feedings)
	actual := make(map[string]Grams, len(rv.Materials))
	for _, f := range b.Feedings {
		actual[f.MaterialID] += f.Grams
	}
	rep.Materials = make([]MaterialReport, len(rv.Materials))
	for i, m := range rv.Materials {
		required := m.Grams * Grams(b.Units)
		act := actual[m.MaterialID]
		rep.Materials[i] = MaterialReport{
			MaterialID: m.MaterialID,
			Required:   required,
			Actual:     act,
			Diff:       act - required,
		}
	}
	return rep, nil
}

// fingerprintOf 计算一次写入操作及其内容的规范指纹，用于请求编号幂等。
func fingerprintOf(op string, args ...any) string {
	raw, err := json.Marshal(struct {
		Op   string `json:"op"`
		Args []any  `json:"args"`
	}{Op: op, Args: args})
	if err != nil {
		panic(fmt.Sprintf("release: 无法序列化操作内容: %v", err))
	}
	return string(raw)
}
