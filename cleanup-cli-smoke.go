package main

import (
 "context"
 "encoding/json"
 "fmt"
 "os"
 "os/exec"
 "path/filepath"
 "strings"
 "time"

 "github.com/gitmoot/gitmoot/internal/config"
 "github.com/gitmoot/gitmoot/internal/db"
 "github.com/gitmoot/gitmoot/internal/workflow"
)

func must(err error) { if err != nil { panic(err) } }
func main() {
 ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second); defer cancel()
 base, err := os.MkdirTemp("", "gitmoot-2358-cli-smoke-"); must(err); defer os.RemoveAll(base)
 home, checkout := filepath.Join(base,"home"), filepath.Join(base,"checkout")
 must(os.MkdirAll(checkout,0700))
 must(os.Setenv("HERDR_SOCKET_PATH", filepath.Join(base,"herdr-disabled")))
 must(os.Unsetenv("HERDR_ENV"))
 must(os.Setenv("GIT_CONFIG_GLOBAL", "/dev/null"))
 must(os.Setenv("GIT_CONFIG_SYSTEM", "/dev/null"))
 git := func(args ...string) string { c:=exec.CommandContext(ctx,"git",args...); c.Dir=checkout; out,err:=c.CombinedOutput(); if err!=nil {panic(fmt.Sprintf("git %v: %s: %v",args,out,err))}; return strings.TrimSpace(string(out)) }
 git("init","--initial-branch=main"); git("config","user.email","smoke@example.com"); git("config","user.name","Smoke")
 must(os.WriteFile(filepath.Join(checkout,"result.txt"),[]byte("producer input\n"),0600)); git("add","result.txt"); git("commit","-m","fixture")
 paths:=config.PathsForHome(home); must(config.Initialize(paths)); store,err:=db.Open(paths.Database); must(err); defer store.Close()
 must(store.UpsertRepo(ctx,db.Repo{Owner:"owner",Name:"repo",DefaultBranch:"main",CheckoutPath:checkout}))
 const id="smoke-producer"
 path,err:=workflow.DelegationWorktreePath(paths.Home,"owner/repo",id,"readonly-seat",0); must(err)
 git("worktree","add","--detach",path,"HEAD")
 payload:=workflow.JobPayload{Repo:"owner/repo",Branch:"main",PullRequest:42,HeadSHA:git("rev-parse","HEAD"),WorktreePath:path,ReadOnlyWorktree:true,ReadOnlySeat:true,Result:&workflow.AgentResult{Decision:"approved",Summary:"immutable terminal evidence"}}
 encoded,err:=json.Marshal(payload); must(err)
 must(store.CreateJob(ctx,db.Job{ID:id,Agent:"reviewer",Type:"review",State:"succeeded",Payload:string(encoded)}))
 cli:=func(want int, args ...string) []byte { full:=append([]string{"job","cleanup",args[0],"--home",home,"--json"},args[1:]...); c:=exec.CommandContext(ctx,os.Args[1],full...); out,err:=c.Output(); code:=0; if err!=nil {if ee,ok:=err.(*exec.ExitError);ok {code=ee.ExitCode(); if code!=want {panic(fmt.Sprintf("CLI %v: exit %d stderr=%s stdout=%s",full,code,ee.Stderr,out))}} else {panic(err)}}; if code!=want {panic(fmt.Sprintf("CLI exit %d want %d: %s",code,want,out))}; return out }
 var inspection workflow.CleanupInspection
 must(json.Unmarshal(cli(0,"inspect",id),&inspection)); if !inspection.Eligible {panic(fmt.Sprintf("inspection refused: %+v",inspection))}
 var applied workflow.CleanupApplyResult
 must(json.Unmarshal(cli(0,"apply","--resource",inspection.ResourceID,"--token",inspection.EligibilityToken,id),&applied))
 if applied.State!="removed" || applied.Receipt==nil {panic(fmt.Sprintf("apply: %+v",applied))}
 if _,err:=os.Stat(path); !os.IsNotExist(err) {panic("workspace not removed")}
 receipt,err:=store.GetCleanupReceipt(ctx,inspection.ResourceID,inspection.LifecycleGeneration); must(err)
 if receipt.RemovedAt=="" || !strings.Contains(receipt.OwnerPayload,"immutable terminal evidence") {panic("terminal receipt did not outlive workspace")}
 must(store.UpdateJobState(ctx,id,"failed")); retried,err:=workflow.RetryJob(ctx,store,id); must(err)
 next,err:=workflow.ParseJobPayload(retried.Payload); must(err); next.WorktreePath=path
 git("worktree","add","--detach",path,"HEAD"); must(os.WriteFile(filepath.Join(path,"result.txt"),[]byte("replacement generation\n"),0600))
 encoded,err=json.Marshal(next); must(err); must(store.UpdateJobPayload(ctx,id,string(encoded)))
 var replay workflow.CleanupApplyResult
 must(json.Unmarshal(cli(0,"apply","--resource",inspection.ResourceID,"--token",inspection.EligibilityToken,id),&replay))
 if replay.State!="already_removed" || replay.Receipt==nil || replay.Receipt.ReceiptID!=receipt.ReceiptID {panic("historical receipt changed")}
 content,err:=os.ReadFile(filepath.Join(path,"result.txt")); must(err); if string(content)!="replacement generation\n" {panic("historical replay touched replacement")}
 next.Result=&workflow.AgentResult{Decision:"approved",Summary:"new generation result"}; encoded,err=json.Marshal(next); must(err); must(store.UpdateJobPayload(ctx,id,string(encoded))); must(store.UpdateJobState(ctx,id,"succeeded"))
 var newInspection workflow.CleanupInspection
 must(json.Unmarshal(cli(0,"inspect",id),&newInspection)); if !newInspection.Eligible {panic(fmt.Sprintf("new inspection refused: %+v",newInspection))}
 must(store.UpdateJobState(ctx,id,"failed")); newest,err:=workflow.RetryJob(ctx,store,id); must(err)
 newestPayload,err:=workflow.ParseJobPayload(newest.Payload); must(err); newestPayload.WorktreePath=path; encoded,err=json.Marshal(newestPayload); must(err); must(store.UpdateJobPayload(ctx,id,string(encoded)))
 var stale workflow.CleanupApplyResult
 must(json.Unmarshal(cli(3,"apply","--resource",newInspection.ResourceID,"--token",newInspection.EligibilityToken,id),&stale))
 if stale.State!="refused" {panic(fmt.Sprintf("stale request: %+v",stale))}
 content,err=os.ReadFile(filepath.Join(path,"result.txt")); must(err); if string(content)!="replacement generation\n" {panic("stale request touched replacement")}
 must(json.NewEncoder(os.Stdout).Encode(map[string]any{"isolated_home":home,"schema_version":inspection.SchemaVersion,"resource_id":inspection.ResourceID,"initial_apply":applied.State,"receipt_id":receipt.ReceiptID,"durable_terminal_evidence":true,"historical_replay":replay.State,"replacement_preserved":true,"stale_generation_state":stale.State,"stale_generation_reasons":stale.Decision.ReasonCodes,"live_data_touched":false}))
}
