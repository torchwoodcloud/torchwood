package testutil

import "testing"

func TestReplaceDatabaseName(t *testing.T) {
	got, err := replaceDatabaseName("postgres://torchwood:torchwood@127.0.0.1:5433/TORCHWOOD_test?sslmode=disable", "TORCHWOOD_test_1_1")
	if err != nil {
		t.Fatalf("replaceDatabaseName: %v", err)
	}
	want := "postgres://torchwood:torchwood@127.0.0.1:5433/TORCHWOOD_test_1_1?sslmode=disable" // #nosec G101 -- 测试断言字符串
	if got != want {
		t.Fatalf("replaceDatabaseName() = %q, want %q", got, want)
	}
}

func TestTestDBPrefix(t *testing.T) {
	t.Setenv("TORCHWOOD_TEST_DATABASE_SOURCE", "postgres://torchwood:torchwood@127.0.0.1:5433/custom_test?sslmode=disable")
	if got := testDBPrefix(); got != "custom_test" {
		t.Fatalf("testDBPrefix() = %q, want custom_test", got)
	}
}
