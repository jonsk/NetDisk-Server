// Package model 定义与 DDL 严格对应的实体(6.4)。
//
// 说明:本包刻意与 sqlc 生成物解耦 —— 迁移脚本是 schema 唯一来源,
// 这里只声明领域模型;sqlc 生成物用于查询映射(sqlc.yaml 尚未启用前手写 SQL)。
package model

import "time"

// 用户角色(4.1)
const (
	RoleSuperAdmin = "super_admin"
	RoleDeptAdmin  = "dept_admin"
	RoleUser       = "user"
)

// 用户状态(4.1)
const (
	StatusActive   = "active"
	StatusDisabled = "disabled"
	StatusPending  = "pending"
)

// 空间类型(4.3 统一空间模型:个人盘 = kind=personal)
const (
	SpacePersonal = "personal"
	SpaceTeam     = "team"
)

// 空间成员权限(4.3 三级)
const (
	PermManager = "manager"
	PermEditor  = "editor"
	PermReader  = "reader"
)

// ValidPermission 判断权限取值是否合法。
//
// 放在 model 层而不是各 service 里各自判断:权限字符串是**数据库 CHECK 约束
// 与业务校验共用的同一个集合**,两处不一致会出现"业务放行、DB 拒绝",
// 表现为 500 而不是 400。
func ValidPermission(p string) bool {
	switch p {
	case PermManager, PermEditor, PermReader:
		return true
	}
	return false
}

// PermissionAtLeast 判断权限是否达到要求(manager > editor > reader)。
func PermissionAtLeast(have, need string) bool {
	rank := map[string]int{PermReader: 1, PermEditor: 2, PermManager: 3}
	h, okH := rank[have]
	n, okN := rank[need]
	if !okH || !okN {
		return false
	}
	return h >= n
}

// 变更流条目类型(6.4 / 6.9 sync_feed.kind)。
//
// 取值必须与 `sync_feed` 表上的 CHECK 约束**逐字一致**:
// 两处不一致会出现"业务放行、DB 拒绝",表现为 500 而不是 400。
const (
	FeedCreated  = "created"
	FeedUpdated  = "updated"
	FeedMoved    = "moved"
	FeedDeleted  = "deleted"
	FeedSharedIn = "shared_in"
)

// ValidFeedKind 与表的 CHECK 约束同源。
func ValidFeedKind(k string) bool {
	switch k {
	case FeedCreated, FeedUpdated, FeedMoved, FeedDeleted, FeedSharedIn:
		return true
	}
	return false
}

// User 对应 users 表。
//
// 注意:配额**不再**存在 users 上(4.1 V2.14 起废止),统一挂在 personal 空间行。
type User struct {
	ID           string
	Username     string
	Email        string
	DisplayName  string
	AvatarURL    string
	Role         string
	Status       string
	TokenVersion int64
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// IsActive 判断用户是否可登录。
func (u *User) IsActive() bool { return u.Status == StatusActive }

// IsAdmin 判断是否具备后台角色(4.1)。
func (u *User) IsAdmin() bool {
	return u.Role == RoleSuperAdmin || u.Role == RoleDeptAdmin
}

// Space 对应 spaces 表(4.3)。
type Space struct {
	ID         string
	Kind       string // personal / team
	GroupID    string // team 必填,personal 为空
	OwnerID    string
	Name       string
	QuotaBytes int64 // 0 = 不限制
	UsedBytes  int64
	Frozen     bool
	LastSeq    int64
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// IsPersonal 判断是否个人空间。
func (s *Space) IsPersonal() bool { return s.Kind == SpacePersonal }

// Available 返回剩余额度;-1 表示不限制。
func (s *Space) Available() int64 {
	if s.QuotaBytes == 0 {
		return -1
	}
	return s.QuotaBytes - s.UsedBytes
}

// File 对应 files 表(6.4)。
//
// Etag 是**生成列**,由 PG 计算(不含双引号,R-21);写入时忽略该字段。
type File struct {
	ID         string
	SpaceID    string
	ParentID   string // 根目录为空
	OwnerID    string // 创建者(审计主体),不参与判权
	Name       string
	IsDir      bool
	Size       int64
	MimeType   string
	HashSHA256 string
	Version    int64
	Depth      int
	Etag       string
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// FileObject 对应 file_objects 表(四态,R-06)。
const (
	ObjectLive          = "live"
	ObjectPendingDelete = "pending_delete"
	ObjectDeleting      = "deleting"
	ObjectDeleted       = "deleted"
)

// FileObject 物理对象与引用计数。
type FileObject struct {
	HashSHA256     string
	Size           int64
	StorageBackend string
	ObjectKey      string
	RefCount       int
	State          string
	DeleteAfter    *time.Time
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// Upload 对应 uploads 表(4.3 预留-结算 / 6.10 定稿幂等)。
const (
	UploadReserved  = "reserved"
	UploadFinalized = "finalized"
	UploadReleased  = "released"
	UploadFailed    = "failed"
)

// Upload 上传任务与额度预留载体。
type Upload struct {
	ID           string
	UserID       string
	SpaceID      string
	ParentID     string
	Name         string
	DeclaredSize int64
	DeclaredHash string
	ActualSize   int64
	ActualHash   string
	State        string
	TargetFileID string
	// TicketHash 是上传凭据的 SHA-256(hex),**明文绝不入库**(6.10)
	TicketHash string
	// UploadedBytes 是已落盘字节数(TUS HEAD 的 Upload-Offset 来源)
	UploadedBytes int64
	// AllowOverwrite 记住「这次要覆盖同名文件」的意图(见 finalize.Input.AllowOverwrite)。
	// 必须落库:TUS 可续传,建任务与定稿可能跨进程/隔很久,定稿时若不知道意图
	// 就只能按默认语义回 409 —— 「改本地大文件」会在最后一片才失败,前面的字节白传。
	AllowOverwrite bool
	ExpiresAt     time.Time
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// 部门来源(4.2:身份平台同步的部门与手工建的部门共存,靠 source+ext_id 对齐)。
const (
	DeptSourceManual   = "manual"
	DeptSourceWeCom    = "wecom"
	DeptSourceDingTalk = "dingtalk"
	DeptSourceLDAP     = "ldap"
	DeptSourceOIDCSCIM = "oidc-scim"
)

// Department 是组织架构节点(4.2)。
//
// **树结构真相在 ParentID**,闭包表只是派生索引 —— 因此全量重建闭包表是
// 幂等且可随时重跑的操作(6.4 T1-4 弃 ltree 的直接收益)。
type Department struct {
	ID        string
	ParentID  string // 根为空
	Name      string
	SortOrder int
	Source    string // manual/wecom/dingtalk/ldap/oidc-scim
	ExtID     string // 源端 id(手工建的为空)
	CreatedAt time.Time
	UpdatedAt time.Time
}

// IsRoot 判断是否为组织树根节点。
func (d *Department) IsRoot() bool { return d.ParentID == "" }
