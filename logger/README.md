# tinysa-logger

Windows単一exe (依存なし)。tinySA Ultra をUSB接続して常時掃引し、CSVに記録。

- `start-logging.bat` ダブルクリックで開始 (COMポート自動検出, 切断時は自動再接続)
- 出力: `logs\YYYY-MM-DD_max.csv` / `_avg.csv` (1行=60秒分のmax-hold / 平均, 列=周波数Hz, 値=dBm)
- 既定掃引: 500kHz–6GHz (5バンド x 450点)。1周約12秒
- 数日溜めたら昼夜比較:
  `tinysa-logger.exe report -day 9-17 -night 22-5` (-kind max も可)
- オプション: `log -port COM5 -bands 500k:30M,30M:300M:900 -interval 60`
- `tinysa-logger.exe ports` でポート一覧

注意: PCのスリープ/省電力を無効にすること。USBケーブルは短く。
Windows側でドライバ要確認 (通常はWindows10以降標準でCOM認識)。
