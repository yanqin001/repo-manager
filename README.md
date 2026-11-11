# repo-manager

repo-manager 把公开 Git 仓库同步到共享的 `/source/<domain_name>/<repo_name>`。默认同步上游所有分支和标签，工作目录检出上游默认分支。SDK 返回实际分配的目录，目录发生重名时会追加稳定的哈希后缀。受管工作树中的本地修改可能在同步重建时被丢弃。

## 启动

```bash
go build -buildvcs=false -o repo-manager ./cmd/repo-manager
./repo-manager -config config/config.yaml
```

服务端运行于 Linux，需要 Git、SSH、本地雷速客户端，以及对仓库根目录和已登记软链接目标的写权限。仓库目录所在文件系统需要支持 Linux 的目录原子交换。服务在 `local_repo_path/.repo-manager` 中保存仓库身份和目录映射，并持有独占进程锁。每个共享仓库根目录运行一个写入服务。

默认监听 `127.0.0.1:8080`。跨主机调用时，通过 `listen_addr` 配置内网监听地址，并确保调用方能读取同一份仓库目录。HTTP API 无应用层鉴权。

- `direct.proxy` 用于国外仓库。国内仓库 `gitee.com` 和 `gitcode.com` 直连。`direct.max_concurrency` 限制本地 Git 并发，`direct.proxy_timeout` 默认 `1h`，任务总超时 `job_timeout` 默认 `6h`。
- 国外仓库走代理失败时，使用 `remote.host` 经 SSH 更新 `remote.repo_path` 下的 Git 缓存。SSH 使用部署账号的密钥和 `known_hosts`，配置中省略 SSH 用户时使用 `root`。远端需要 Git、`sha256sum` 和 `rayfile.remote_cmd_path` 指定的客户端。
- 雷速服务端按 `rayfile.server_path/<domain_name>` 分区，默认根目录为 `/data/code`。确保分区可写、可下载，例如 `/data/code/github.com`。每个任务上传独立命名的 bundle。服务校验 SHA-256 和 Git 前置对象后导入，增量校验失败时重新取全量包。传输完成后尽力清理任务文件。日志记录 bundle 类型、大小和打包、上传、下载耗时。
- GitHub、Gitee 和 GitCode 通过平台 API 返回的 ID 确认改名和组织转移。可选的 `identity_tokens` 以域名为键配置平台 API token。身份解析失败时保留已有映射。仓库已登记过稳定 ID 或可通过请求地址、规范地址接管旧目录时，可以复用原目录。首次只提交迁移后的新地址时，未登记且无法由这些地址定位的旧目录可能无法识别。

## Go SDK

```go
import "github.com/yanqin001/repo-manger/sdk"

client, err := sdk.NewClient("http://127.0.0.1:8080", nil)
if err != nil {
    return err
}
result, err := client.Sync(ctx, "https://github.com/torvalds/linux")
if err != nil {
    return err
}
fmt.Println(result.LocalPath, result.Commit)
```

`Sync` 会等待任务完成，取消调用方的 context 只会停止等待。需要异步处理时，可用 `Submit` 获得任务 ID，再用 `GetTask` 查询。相同仓库 URL 的重叠请求共用在途任务，已验证为同一仓库的不同地址按正式目录串行更新。

HTTP 接口为 `POST /v1/sync`，请求体为 `{"url":"https://github.com/torvalds/linux"}`。用 `GET /v1/tasks/{id}` 查询状态和结果。任务状态在进程内保存，成功后的仓库目录和身份记录在重启后可复用。
