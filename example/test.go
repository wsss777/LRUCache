package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	lcache "github.com/wsss777/LRUCache"
)

func main() {
	// 添加命令行参数，用于区分不同节点
	port := flag.Int("port", 8001, "节点端口")
	nodeID := flag.String("node", "A", "节点标识符")
	flag.Parse()

	addr := fmt.Sprintf(":%d", *port)
	log.Printf("[节点%s] 启动，地址: %s", *nodeID, addr)

	// 创建节点
	node, err := lcache.NewServer(addr, "kama-cache",
		lcache.WithEtcdEndpoints([]string{"localhost:2379"}),
		lcache.WithDialTimeout(30*time.Second),
	)
	if err != nil {
		log.Fatal("创建节点失败:", err)
	}

	// 创建节点选择器
	picker, err := lcache.NewClientPicker(addr)
	if err != nil {
		log.Fatal("创建节点选择器失败:", err)
	}

	// 创建缓存组
	group := lcache.NewGroup("test", 2<<20, lcache.GetterFunc(
		func(ctx context.Context, key string) ([]byte, error) {
			log.Printf("[节点%s] 触发数据源加载: key=%s", *nodeID, key)
			return []byte(fmt.Sprintf("节点%s的数据源值", *nodeID)), nil
		}),
	)

	// 注册节点选择器
	group.RegisterPeers(picker)

	// 启动节点
	go func() {
		log.Printf("[节点%s] 开始启动服务...", *nodeID)
		if err := node.Start(); err != nil {
			log.Fatal("启动节点失败:", err)
		}
	}()

	// 等待节点注册完成
	log.Printf("[节点%s] 等待节点注册...", *nodeID)
	time.Sleep(5 * time.Second)

	ctx := context.Background()

	// 设置本节点的特定键值对
	localKey := fmt.Sprintf("key_%s", *nodeID)
	localValue := []byte(fmt.Sprintf("这是节点%s的数据", *nodeID))

	fmt.Printf("\n=== 节点%s：设置本地数据 ===\n", *nodeID)
	err = group.Set(ctx, localKey, localValue)
	if err != nil {
		log.Fatal("设置本地数据失败:", err)
	}
	fmt.Printf("节点%s: 设置键 %s 成功\n", *nodeID, localKey)

	// 等待其他节点也完成设置
	log.Printf("[节点%s] 等待其他节点准备就绪...", *nodeID)
	time.Sleep(30 * time.Second)

	// 打印当前已发现的节点
	picker.PrintPeers()

	// 测试获取本地数据
	fmt.Printf("\n=== 节点%s：获取本地数据 ===\n", *nodeID)
	fmt.Printf("直接查询本地缓存...\n")

	// 打印缓存统计信息
	stats := group.Stats()
	fmt.Printf("缓存统计: %+v\n", stats)

	if val, err := group.Get(ctx, localKey); err == nil {
		fmt.Printf("节点%s: 获取本地键 %s 成功: %s\n", *nodeID, localKey, val.String())
	} else {
		fmt.Printf("节点%s: 获取本地键失败: %v\n", *nodeID, err)
	}

	// 测试获取其他节点的数据
	otherKeys := []string{"key_A", "key_B", "key_C"}
	for _, key := range otherKeys {
		if key == localKey {
			continue // 跳过本节点的键
		}
		fmt.Printf("\n=== 节点%s：尝试获取远程数据 %s ===\n", *nodeID, key)
		log.Printf("[节点%s] 开始查找键 %s 的远程节点", *nodeID, key)
		if val, err := group.Get(ctx, key); err == nil {
			fmt.Printf("节点%s: 获取远程键 %s 成功: %s\n", *nodeID, key, val.String())
		} else {
			fmt.Printf("节点%s: 获取远程键失败: %v\n", *nodeID, err)
		}
	}

	// 保持程序运行
	// select {}
	fmt.Println("\n=== 互动测试模式已开启 ===")
	fmt.Println("输入命令后回车：")
	fmt.Println("  get <key>     → 测试获取 key 的值")
	fmt.Println("  set <key> <value> → 测试设置 key-value（本节点）")
	fmt.Println("  stats         → 显示当前缓存统计")
	fmt.Println("  peers         → 显示已发现的节点")
	fmt.Println("  exit / quit   → 退出程序")
	fmt.Println("----------------------------------------")

	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		parts := strings.Fields(line)
		cmd := parts[0]

		switch cmd {
		case "get":
			if len(parts) < 2 {
				fmt.Println("用法: get <key>")
				continue
			}
			key := parts[1]
			val, err := group.Get(ctx, key)
			if err != nil {
				fmt.Printf("❌ Get %q 失败: %v\n", key, err)
			} else {
				fmt.Printf("✅ Get %q → %q\n", key, val.String())
			}

		case "set":
			if len(parts) < 3 {
				fmt.Println("用法: set <key> <value>")
				continue
			}
			key := parts[1]
			value := []byte(strings.Join(parts[2:], " "))
			err := group.Set(ctx, key, value)
			if err != nil {
				fmt.Printf("❌ Set %q 失败: %v\n", key, err)
			} else {
				fmt.Printf("✅ Set %q → %q 成功\n", key, value)
			}

		case "stats":
			stats := group.Stats()
			fmt.Printf("缓存统计:\n%+v\n", stats)

		case "peers":
			picker.PrintPeers()

		case "exit", "quit", "q":
			fmt.Println("退出程序...")
			os.Exit(0)

		default:
			fmt.Println("未知命令，请输入 get / set / stats / peers / exit")
		}
	}

	if err := scanner.Err(); err != nil {
		log.Printf("输入错误: %v", err)
	}
}
