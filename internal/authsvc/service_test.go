package authsvc_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/netdisk/netdisk/internal/apierr"
	"github.com/netdisk/netdisk/internal/auth"
	"github.com/netdisk/netdisk/internal/authsvc"
	"github.com/netdisk/netdisk/internal/model"
)

// 建用户必须**同时**建出 personal 空间与根目录(4.1 V2.14:配额挂在空间行)
func TestCreateUserCreatesPersonalSpaceAndRoot(t *testing.T) {
	f := Setup(t)
	ctx := context.Background()

	sp, err := f.Spaces.PersonalOf(ctx, f.DB.Pool, f.UserID)
	if err != nil {
		t.Fatalf("应有 personal 空间: %v", err)
	}
	if sp.Kind != model.SpacePersonal || sp.OwnerID != f.UserID {
		t.Errorf("空间属性错误: %+v", sp)
	}
	if sp.UsedBytes != 0 {
		t.Errorf("初始 used_bytes 应为 0, got %d", sp.UsedBytes)
	}
	root, err := f.Files.GetRoot(ctx, f.DB.Pool, sp.ID)
	if err != nil {
		t.Fatalf("应有根目录: %v", err)
	}
	if !root.IsDir || root.ParentID != "" {
		t.Errorf("根目录属性错误: %+v", root)
	}
}

// 登录 → refresh 轮换 → 登出 全链路;refresh 必须落库且轮换后旧值失效
func TestLoginRefreshLogoutFlow(t *testing.T) {
	f := Setup(t)
	ctx := context.Background()

	res, err := f.Svc.Login(ctx, authsvc.LoginInput{
		Login: f.Login, Password: f.Password, Audience: auth.AudienceDesktop,
	})
	if err != nil {
		t.Fatalf("登录应成功: %v", err)
	}
	if res.Token.AccessToken == "" || res.Token.RefreshToken == "" {
		t.Fatal("应签发双 token")
	}
	if res.User.ID != f.UserID || res.Space == nil {
		t.Errorf("登录结果应含正确用户与个人空间")
	}

	actor, err := f.Tokens.Verify(ctx, res.Token.AccessToken)
	if err != nil {
		t.Fatalf("access 校验失败: %v", err)
	}
	if actor.Audience != auth.AudienceDesktop {
		t.Errorf("audience 应为 desktop, got %q", actor.Audience)
	}

	state, err := f.Users.GetRefresh(ctx, f.DB.Pool, res.Token.RefreshJTI)
	if err != nil {
		t.Fatalf("refresh 应已落库(权威账本): %v", err)
	}
	if state.RevokedAt != nil {
		t.Error("新签发的 refresh 不应已吊销")
	}

	second, err := f.Svc.Refresh(ctx, res.Token.RefreshToken, auth.AudienceDesktop)
	if err != nil {
		t.Fatalf("refresh 应成功: %v", err)
	}
	if _, err := f.Svc.Refresh(ctx, res.Token.RefreshToken, auth.AudienceDesktop); err == nil {
		t.Fatal("已轮换的旧 refresh 必须失效")
	}
	if _, err := f.Svc.Refresh(ctx, second.RefreshToken, auth.AudienceDesktop); err != nil {
		t.Fatalf("新 refresh 应可用: %v", err)
	}

	if err := f.Svc.Logout(ctx, second.RefreshToken); err != nil {
		t.Fatalf("登出应成功: %v", err)
	}
	if _, err := f.Svc.Refresh(ctx, second.RefreshToken, ""); err == nil {
		t.Fatal("登出后的 refresh 必须失效")
	}
	// 登出幂等:重复登出不应报错
	if err := f.Svc.Logout(ctx, second.RefreshToken); err != nil {
		t.Errorf("重复登出应幂等: %v", err)
	}
}

// 失败尝试:统一错误防枚举 + 达阈值锁定
func TestLoginFailureLockout(t *testing.T) {
	f := Setup(t)
	ctx := context.Background()
	f.Svc.FailThreshold = 3
	f.Svc.LockDuration = time.Minute

	_, errNoUser := f.Svc.Login(ctx, authsvc.LoginInput{Login: "no_such_user_xyz", Password: "whatever1"})
	_, errBadPwd := f.Svc.Login(ctx, authsvc.LoginInput{Login: f.Login, Password: "wrongpass1"})
	if errNoUser == nil || errBadPwd == nil {
		t.Fatal("两种失败都应报错")
	}
	if errNoUser.Error() != errBadPwd.Error() {
		t.Errorf("用户不存在与口令错误必须对外一致(防枚举):\n  no_user=%v\n  bad_pwd=%v", errNoUser, errBadPwd)
	}

	_, _ = f.Svc.Login(ctx, authsvc.LoginInput{Login: f.Login, Password: "wrongpass2"})
	_, _ = f.Svc.Login(ctx, authsvc.LoginInput{Login: f.Login, Password: "wrongpass3"})

	_, err := f.Svc.Login(ctx, authsvc.LoginInput{Login: f.Login, Password: f.Password})
	if err == nil {
		t.Fatal("锁定期间即使口令正确也应拒绝")
	}
	var ae *apierr.Error
	if !errors.As(err, &ae) || ae.Code != apierr.CodeRateLimited {
		t.Errorf("应返回 rate_limited(账号锁定), got %v", err)
	}
}

// 改密后所有会话立即失效(6.8 纪律 3)
func TestPasswordChangeRevokesSessions(t *testing.T) {
	f := Setup(t)
	ctx := context.Background()

	res, err := f.Svc.Login(ctx, authsvc.LoginInput{Login: f.Login, Password: f.Password, Audience: auth.AudienceWeb})
	if err != nil {
		t.Fatalf("登录: %v", err)
	}
	oldVersion := res.User.TokenVersion

	newPass := "N3wP@ssw0rd!"
	if err := f.Svc.SetPassword(ctx, f.UserID, newPass); err != nil {
		t.Fatalf("改密: %v", err)
	}

	u, err := f.Users.GetByID(ctx, f.DB.Pool, f.UserID)
	if err != nil {
		t.Fatalf("读取用户: %v", err)
	}
	if u.TokenVersion <= oldVersion {
		t.Errorf("改密应自增 token_version: %d -> %d", oldVersion, u.TokenVersion)
	}
	if _, err := f.Svc.Refresh(ctx, res.Token.RefreshToken, ""); err == nil {
		t.Fatal("改密后旧 refresh 必须失效")
	}
	if _, err := f.Svc.Login(ctx, authsvc.LoginInput{Login: f.Login, Password: newPass, Audience: auth.AudienceWeb}); err != nil {
		t.Errorf("新口令应可登录: %v", err)
	}
	if _, err := f.Svc.Login(ctx, authsvc.LoginInput{Login: f.Login, Password: f.Password, Audience: auth.AudienceWeb}); err == nil {
		t.Error("旧口令不应再可登录")
	}
}

// 停用账号不可登录(4.1)
func TestDisabledUserCannotLogin(t *testing.T) {
	f := Setup(t)
	ctx := context.Background()
	if err := f.Users.SetStatus(ctx, f.DB.Pool, f.UserID, model.StatusDisabled); err != nil {
		t.Fatalf("停用: %v", err)
	}
	_, err := f.Svc.Login(ctx, authsvc.LoginInput{Login: f.Login, Password: f.Password, Audience: auth.AudienceWeb})
	if err == nil {
		t.Fatal("停用账号不应可登录")
	}
	var ae *apierr.Error
	if !errors.As(err, &ae) || ae.Status != 403 {
		t.Errorf("应为 403, got %v", err)
	}
}

// H5 短时效 + 跨端 refresh 拒绝(2.7 / R-14)
func TestH5AudienceShortTTL(t *testing.T) {
	f := Setup(t)
	res, err := f.Svc.Login(context.Background(), authsvc.LoginInput{
		Login: f.Login, Password: f.Password, Audience: auth.AudienceDesktop,
	})
	if err != nil {
		t.Fatalf("登录: %v", err)
	}
	if res.Token.ExpiresIn != int((5 * time.Minute).Seconds()) {
		t.Errorf("H5 access 应为 5 分钟, got %ds", res.Token.ExpiresIn)
	}
	if res.Token.RefreshIn != int((12 * time.Hour).Seconds()) {
		t.Errorf("H5 refresh 应为 12 小时, got %ds", res.Token.RefreshIn)
	}
	if _, err := f.Svc.Refresh(context.Background(), res.Token.RefreshToken, auth.AudienceWeb); err == nil {
		t.Error("H5 refresh 不应能在 web 端使用")
	}
}
