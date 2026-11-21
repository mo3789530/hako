package agenttask

import "testing"

func TestSHA256BindsEveryTaskFieldAndIsDeterministic(t *testing.T) {
	base := Context{TenantID: "tenant_1", WorkloadRunID: "run_1", IntegrationID: "integration_1",
		DeliveryID: "delivery-1", InstallationID: 2, RepositoryID: 3, IssueNumber: 4, CommentID: 5,
		DefaultBranch: "main", CommitSHA: "abc", Title: "Issue", Body: "Task"}
	want := SHA256(base)
	if want == "" || SHA256(base) != want {
		t.Fatal("same canonical task did not produce a stable digest")
	}
	mutations := []func(*Context){
		func(v *Context) { v.TenantID = "tenant_other" },
		func(v *Context) { v.WorkloadRunID = "run_other" },
		func(v *Context) { v.IntegrationID = "integration_other" },
		func(v *Context) { v.DeliveryID = "delivery-other" },
		func(v *Context) { v.InstallationID++ },
		func(v *Context) { v.RepositoryID++ },
		func(v *Context) { v.IssueNumber++ },
		func(v *Context) { v.CommentID++ },
		func(v *Context) { v.DefaultBranch = "release" },
		func(v *Context) { v.CommitSHA = "def" },
		func(v *Context) { v.Title = "Other issue" },
		func(v *Context) { v.Body = "Other task" },
	}
	for index, mutate := range mutations {
		changed := base
		mutate(&changed)
		if got := SHA256(changed); got == want {
			t.Errorf("mutation %d was not bound by the digest", index)
		}
	}
}
