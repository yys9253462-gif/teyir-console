-- 004: 接入 hysteria 自带的 trafficStats 接口，拿真正的上/下行速率。
-- trafficStats 是「整个 hysteria 进程」级别的计数，且带 ?clear=true 的读取语义是
-- 「取走并清零」，天然就是速率采样，因此按采集周期落 traffic_samples(source='traffic_api')。
-- 密钥不落库：库里只记目标机上那个 root-only 文件的路径，读取动作在目标机本地完成。
ALTER TABLE servers ADD COLUMN IF NOT EXISTS traffic_api_url TEXT;
ALTER TABLE servers ADD COLUMN IF NOT EXISTS traffic_secret_file TEXT;
