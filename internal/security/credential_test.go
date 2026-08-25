package security

import (
	"encoding/hex"
	"strings"
	"testing"

	"github.com/vance1852/trail-permit-dispatch/internal/apperr"
)

func TestPBKDF2MatchesKnownVector(t *testing.T) {
	// Known PBKDF2-HMAC-SHA256 vector: password "password", salt "salt",
	// 1 iteration, 32 byte output.
	derived := pbkdf2SHA256([]byte("password"), []byte("salt"), 1, 32)
	expected := "120fb6cffcf8b32c43e7225256c4f837a86548c92ccc35480805987cb70be17b"
	if got := hex.EncodeToString(derived); got != expected {
		t.Fatalf("单次迭代派生结果不符: %s", got)
	}
	twoRounds := pbkdf2SHA256([]byte("password"), []byte("salt"), 2, 32)
	expectedTwo := "ae4d0c95af6b46d32d0adff928f06dd02a303f8ef3c251dfd6e2d85a95474c43"
	if got := hex.EncodeToString(twoRounds); got != expectedTwo {
		t.Fatalf("两次迭代派生结果不符: %s", got)
	}
	long := pbkdf2SHA256([]byte("password"), []byte("salt"), 2, 48)
	if len(long) != 48 {
		t.Fatalf("跨块派生长度错误: %d", len(long))
	}
	if hex.EncodeToString(long[:32]) != expectedTwo {
		t.Fatal("跨块派生的首块应与单块结果一致")
	}
}

func TestHashAndVerifyRoundTrip(t *testing.T) {
	hasher := NewHasher(2048)
	credential, err := hasher.Hash("trailpermit2026")
	if err != nil {
		t.Fatalf("派生密码失败: %v", err)
	}
	if credential.Iterations != 2048 {
		t.Fatalf("迭代次数未保存: %d", credential.Iterations)
	}
	if len(credential.Salt) != 32 || len(credential.Hash) != 64 {
		t.Fatalf("盐值或摘要长度异常: salt=%d hash=%d", len(credential.Salt), len(credential.Hash))
	}
	if err := hasher.Verify(credential, "trailpermit2026"); err != nil {
		t.Fatalf("正确密码校验失败: %v", err)
	}
	err = hasher.Verify(credential, "trailpermit2027")
	if !apperr.Is(err, apperr.CodeUnauthenticated) {
		t.Fatalf("错误密码应返回 unauthenticated，实际 %v", err)
	}

	other, err := hasher.Hash("trailpermit2026")
	if err != nil {
		t.Fatalf("二次派生失败: %v", err)
	}
	if other.Hash == credential.Hash {
		t.Fatal("相同密码在不同盐值下不应产生相同摘要")
	}
}

func TestVerifyRejectsCorruptedStoredMaterial(t *testing.T) {
	hasher := NewHasher(1024)
	credential, err := hasher.Hash("trailpermit2026")
	if err != nil {
		t.Fatalf("派生密码失败: %v", err)
	}
	broken := credential
	broken.Salt = "zz-not-hex"
	if err := hasher.Verify(broken, "trailpermit2026"); !apperr.Is(err, apperr.CodeInternal) {
		t.Fatalf("损坏的盐值应返回 internal，实际 %v", err)
	}
	broken = credential
	broken.Hash = "not-hex"
	if err := hasher.Verify(broken, "trailpermit2026"); !apperr.Is(err, apperr.CodeInternal) {
		t.Fatalf("损坏的摘要应返回 internal，实际 %v", err)
	}
}

func TestHasherDefaultsAndRehashDetection(t *testing.T) {
	hasher := NewHasher(0)
	if hasher.Iterations() != DefaultIterations {
		t.Fatalf("非法迭代次数应回退到默认值，实际 %d", hasher.Iterations())
	}
	legacy := NewHasher(1024)
	credential, err := legacy.Hash("trailpermit2026")
	if err != nil {
		t.Fatalf("派生密码失败: %v", err)
	}
	stronger := NewHasher(4096)
	if !stronger.NeedsRehash(credential) {
		t.Fatal("低成本凭据应被判定需要重算")
	}
	if legacy.NeedsRehash(credential) {
		t.Fatal("同成本凭据不应被判定需要重算")
	}
	// A stored credential without an iteration count falls back to the hasher
	// setting instead of failing verification.
	credential.Iterations = 0
	if err := legacy.Verify(credential, "trailpermit2026"); err != nil {
		t.Fatalf("缺少迭代次数时应回退到当前设置: %v", err)
	}
}

func TestTokenMintingIsOpaqueAndHashed(t *testing.T) {
	first, err := NewToken()
	if err != nil {
		t.Fatalf("生成令牌失败: %v", err)
	}
	second, err := NewToken()
	if err != nil {
		t.Fatalf("生成令牌失败: %v", err)
	}
	if first.Plaintext == second.Plaintext {
		t.Fatal("两次生成的令牌不应相同")
	}
	if strings.Contains(first.Hash, first.Plaintext) {
		t.Fatal("存储哈希不得包含明文令牌")
	}
	if first.Hash != HashToken(first.Plaintext) {
		t.Fatal("令牌哈希应可由明文重现")
	}
	if len(first.Hash) != 64 {
		t.Fatalf("令牌哈希应为 64 位十六进制，实际 %d", len(first.Hash))
	}
}

func TestNewReferenceFormat(t *testing.T) {
	reference, err := NewReference("PF", 4)
	if err != nil {
		t.Fatalf("生成流水号失败: %v", err)
	}
	if !strings.HasPrefix(reference, "PF-") {
		t.Fatalf("流水号前缀错误: %s", reference)
	}
	if len(reference) != len("PF-")+8 {
		t.Fatalf("流水号长度错误: %s", reference)
	}
	fallback, err := NewReference("TP", 0)
	if err != nil {
		t.Fatalf("生成流水号失败: %v", err)
	}
	if len(fallback) != len("TP-")+12 {
		t.Fatalf("默认长度流水号错误: %s", fallback)
	}
}
