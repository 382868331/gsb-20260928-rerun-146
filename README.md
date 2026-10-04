# assetpack — 数字资产资源打包系统

面向本地数字素材的**内容寻址**打包/解包库与 CLI。它不是通用压缩工具：
核心是资源身份、引用关系与版本化增量发布。

## 特性

- **内容身份**：每个资源 ID 为其规范化内容的 SHA-256；片段同样按 SHA-256 寻址。
- **共享去重**：相同内容片段在包内只保存一份，多个资源用引用指向它，引用关系不丢。
- **依赖关系**：清单记录每个资源 `dependsOn` 的逻辑路径；循环依赖可完整恢复。
- **按需读取**：读取器先建片段偏移索引，只在用到某片段时才从底层 `ReaderAt` 读取，
  逐片段核对大小与摘要，再核对整体内容身份。未被损坏片段影响的资源仍可读。
- **原子发布**：包尾存放索引偏移、长度与摘要；解包先在内存核对全部资源，
  通过后才原子（临时文件 + rename）写出素材和 `.assetpack/manifest.json` 关系索引。
- **路径越界拒绝**：`..`、绝对路径、保留名 `.assetpack` 在打包与解包两处都被拒绝；解包通过根目录句柄逐级解析真实路径，预置或运行期替换的越界符号链接同样无法写出根目录。
- **嵌套资源**：目录作为合成资源保存稳定的子项清单（排序、确定性）。
- **增量发布**：增量包记录 `baseIndex`（上一版索引身份），内容身份不变的片段不重复保存，
  按 `base -> delta...` 组成包链读取合并视图；离开基础包无法单独打开增量包。
- **包内删除**：增量 spec 中 `{"path": "...", "remove": true}` 写入墓碑，包链合并后该资源从新版本消失。
- **更新预览**：`PreviewUpdate` 对比包链清单与本地安装索引，区分可直接替换、本地已改动、
  包内删除与新增；本地已改动的文件绝不静默覆盖，必须人工选择保留本地版本或采用包内版本，
  并说明该选择对其它资源引用的影响（引用解析到哪个内容身份、删除后哪些引用悬空）。
- **采用与接续**：`ApplyUpdate` 正式采用前再次核对预览依据，期间变化的文件要求重新决定；
  采用过程记录日志，索引在最后原子提交，中断不会留下指向未完整写入内容的索引，
  `ResumeUpdate` 可重开接续。
- **撤回与溯源**：`RollbackUpdate` 撤回本次更新、恢复更新前索引与内容，人工选择记录保留在
  `.assetpack/choices.json`；最终清单中每个资源标注来自原安装、本地保留还是此次更新。
  路径边界检查在预览、采用和撤回三个阶段同样有效。
- **确定性**：固定输入（含片段大小）产出字节一致的包与稳定索引摘要；不含时间戳。

## 包格式 (v1, 大端序)

```
magic "ASTPKG01" | u32 version | u64 fragmentSize | u64 fragmentCount | u64 manifestLen
fragment*: "FRAG" | u8 digestLen | digest | u64 dataLen | data   // digest 排序、去重
manifest JSON                                                  // 索引, 最后写入
trailer: u64 indexOffset | u64 manifestLen | 32B manifestDigest | "ASTPKEND"
```

## 用法

```sh
# 首轮打包
assetpack pack -spec base.json -out base.pkg
# 校验(片段/大小/摘要)
assetpack verify -pkg base.pkg
# 解包(先核对再原子发布, 恢复引用关系)
assetpack unpack -pkg base.pkg -dir out/

# 增量发布: 沿用同一内容身份, 只放变化/新增资源
assetpack pack -spec delta.json -base base.pkg -out delta.pkg
assetpack verify -base base.pkg -pkg delta.pkg
assetpack unpack -base base.pkg -pkg delta.pkg -dir out2/

# 应用到已有安装: 先预览, 再带人工决策采用, 可中断接续与撤回
assetpack preview -base base.pkg -pkg delta.pkg -dir out2/ -out plan.json
assetpack apply -base base.pkg -pkg delta.pkg -dir out2/ -plan plan.json -decisions dec.json
assetpack apply -base base.pkg -pkg delta.pkg -dir out2/ -resume   # 中断后重开接续
assetpack rollback -dir out2/
```

spec JSON 示例：

```json
{
  "fragmentSize": 4096,
  "resources": [
    {"path": "scenes/a.json", "file": "a.json", "dependsOn": ["scenes/b.json"]},
    {"path": "scenes/b.json", "inline": "{}", "dependsOn": ["scenes/a.json"]},
    {"path": "scenes", "dir": true, "children": ["scenes/a.json", "scenes/b.json"]}
  ]
}
```

## 库 API

- `Build(reqs, Options{Base: prevManifest}) ([]byte, *Manifest, error)`
- `OpenReaderAt(ra, size)` / `OpenChain([]ReaderAtSize)`
- `(*Reader).ReadResource(path)` / `VerifyAll()` / `Manifest()`
- `Unpack(reader, rootDir)`
- `WriteFileAtomic(path, data, perm)`
- `PreviewUpdate(reader, rootDir)` / `(*UpdatePlan).Decide(path, decision)`
- `ApplyUpdate(reader, rootDir, plan)` / `ResumeUpdate(reader, rootDir)` / `RollbackUpdate(rootDir)`

## 测试

```sh
go test ./...
```

覆盖：共享去重、循环依赖恢复、嵌套目录、确定性、损坏片段按需检测、
真实路径/符号链接越界拒绝、重复资源拒绝、未完成不发布索引、增量身份复用与包链合并、截断/尾部篡改。
更新流程覆盖：预览分类（替换/冲突/删除/新增）、引用影响说明、采用前复核过期拒绝、
中断后索引不被污染与重开接续、撤回恢复与人工选择保留、越界路径在预览阶段拒绝。
