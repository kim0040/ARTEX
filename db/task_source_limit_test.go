package db

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestCreateTaskRejectsTooManySourcesBeforeOpeningTransaction(t *testing.T) {
	sourceIDs := make([]int64, MaxTaskSourceCount+1)
	for i := range sourceIDs {
		sourceIDs[i] = int64(i + 1)
	}

	// 데이터베이스 핸들은 필요 없다. 검사는 Begin보다 먼저 돌아야
	// 너무 큰 요청이 연결을 잡거나 일부 행을 만들지 못한다.
	_, err := (&DB{}).CreateTaskWithOptions("child", "goal", TaskCreateOptions{SourceTaskIDs: sourceIDs})
	if err == nil || !strings.Contains(err.Error(), "원본 작업이 너무 많습니다") {
		t.Fatalf("expected source-count validation error, got %v", err)
	}
}

func TestNormalizeTaskCompanyIDs(t *testing.T) {
	got, err := NormalizeTaskCompanyIDs([]int64{4, 2, 4, 7, 2})
	if err != nil {
		t.Fatal(err)
	}
	if want := []int64{4, 2, 7}; !reflect.DeepEqual(got, want) {
		t.Fatalf("NormalizeTaskCompanyIDs=%v, want %v", got, want)
	}
	if _, err := NormalizeTaskCompanyIDs([]int64{1, 0}); !errors.Is(err, ErrTaskCompanyIDsInvalid) {
		t.Fatalf("invalid company id error=%v", err)
	}
}

func TestCreateTaskRejectsTooManyCompaniesBeforeOpeningTransaction(t *testing.T) {
	companyIDs := make([]int64, MaxTaskCompanyCount+1)
	for i := range companyIDs {
		companyIDs[i] = int64(i + 1)
	}

	_, err := (&DB{}).CreateTaskWithOptions("child", "goal", TaskCreateOptions{CompanyIDs: companyIDs})
	if !errors.Is(err, ErrTaskCompanyIDsInvalid) {
		t.Fatalf("expected company-count validation error, got %v", err)
	}
}
