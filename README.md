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

## 测试

```sh
go test ./...
```

覆盖：共享去重、循环依赖恢复、嵌套目录、确定性、损坏片段按需检测、
真实路径/符号链接越界拒绝、重复资源拒绝、未完成不发布索引、增量身份复用与包链合并、截断/尾部篡改。
