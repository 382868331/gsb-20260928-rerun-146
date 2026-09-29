# 数字资产资源打包系统

这是一个面向本地数字素材的 Go 内容寻址打包库和命令行工具，不是通用压缩工具。核心目标是为资源、片段、依赖和包版本建立稳定身份，并支持后续增量发布。

## 能力

- 内容身份：资源使用 `sha256:<内容摘要>` 标识；文件按 64 KiB 片段去重存储。
- 共享资源：相同字节只保存一份，但可保留多个逻辑路径。
- 关系恢复：依赖图和嵌套资源引用显式写入清单，支持循环依赖。
- 按需读取：包打开时只验证尾部索引；读取资源时再读取对应片段。
- 损坏检测：每个片段校验偏移、大小和 SHA-256，资源整体再次校验大小和摘要。
- 增量发布：新包引用基础包 ID，未变化资源与片段沿用同一身份，不重复写入旧片段。
- 原子发布：先写临时文件，全部数据和索引完成后重命名；完成前目标路径不发布新索引。
- 安全解包：拒绝绝对路径、反斜杠、`..`、冗余路径、符号链接穿越和非普通文件目标。
- 确定性：固定输入产生相同清单 ID 和字节级相同的包文件。

## 包格式

文件后缀约定为 `.dap`：

```text
DAP1
<按摘要排序的片段字节>
<flate 压缩的 JSON Manifest>
换行 + DAPIDX1 + 8 字节大端索引长度 + 索引 SHA-256 摘要
```

JSON Manifest 包含：

- `package_id`：对不含该字段的规范清单 JSON 计算 SHA-256。
- `version`：从 1 开始的包版本。
- `chunks`：片段摘要、物理偏移和大小。
- `resources`：资源身份、逻辑路径、大小、片段顺序、依赖和嵌套引用。
- `base_id`：增量包的直接基础包 ID；完整包省略。

## 库使用

```go
m, err := assetpack.BuildPackage("v1.dap", []assetpack.InputResource{
    {Paths: []string{"textures/a.png", "textures/shared.png"}, Data: data},
    {Paths: []string{"scene.json"}, Data: scene, Deps: []int{0}},
})

p, _ := assetpack.Open("v1.dap")
defer p.Close()
payload, _ := p.ReadPath("scene.json")
_ = p.Extract(assetpack.ExtractOptions{Root: "out"})
```

增量：

```go
base := openedBase.Manifest()
delta, _ := assetpack.BuildDelta("v2.dap", &base, 2, nextInputs)
chain, _ := assetpack.NewChain(openedV2, openedV1)
unchanged, _ := chain.ReadPath("textures/shared.png")
```

## 命令行

素材描述使用本地 JSON，不访问外部站点：

```json
{
  "resources": [
    {"paths": ["art/palette.bin"], "source": "palette.bin"},
    {"paths": ["scene/root.json"], "source": "root.json", "depends_on": [0]}
  ]
}
```

命令：

```sh
go run ./cmd/assetpack pack -spec examples/spec.json -o v1.dap
go run ./cmd/assetpack list v1.dap
go run ./cmd/assetpack cat v1.dap art/palette.bin
go run ./cmd/assetpack extract -root out v1.dap
go run ./cmd/assetpack pack -spec examples/spec-v2.json -base v1.dap -version 2 -o v2.dap
```

## 测试

```sh
go test ./...
```

测试覆盖共享内容、循环依赖、嵌套资源、超过 64 KiB 的多片段资源、片段损坏、路径越界、原子发布、确定性输出和增量身份复用。
