# op-ctl

OP-Stack L2 paychain CLI. Inspects op-node / op-geth backends defined in `config/*.toml`.

## 설치

### 요구 사항

- Go 1.25 이상

### 빌드

```sh
git clone <repo-url> op-ctl
cd op-ctl
make build
```

빌드가 끝나면 프로젝트 루트에 `./op-ctl` 바이너리가 생성됩니다.

### 설정

`config/` 디렉토리 안에 체인별 TOML 파일을 둡니다.

```sh
mkdir -p config
cp config.example/config.pp-testnet.toml config/
$EDITOR config/config.pp-testnet.toml
```

여러 체인 설정을 두면 `op-ctl` 실행 시 픽커가 떠서 선택할 수 있습니다.

### 실행

```sh
./op-ctl
```
