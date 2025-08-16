package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"

	"github.com/robfig/cron/v3"
	"github.com/ztc1997/ikuai-bypass/api"
	"github.com/ztc1997/ikuai-bypass/router"
	"gopkg.in/yaml.v3"
)

var confPath = flag.String("c", "./config.yml", "配置文件路径")

var conf struct {
	IkuaiURL  string `yaml:"ikuai-url"`
	Username  string `yaml:"username"`
	Password  string `yaml:"password"`
	Cron      string `yaml:"cron"`
	CustomIsp []struct {
		Name string `yaml:"name"`
		URL  string `yaml:"url"`
	} `yaml:"custom-isp"`
	IpGroup []struct {
		Name string `yaml:"name"`
		URL  string `yaml:"url"`
	} `yaml:"ip-group"`
	StreamDomain []struct {
		Interface string `yaml:"interface"`
		SrcAddr   string `yaml:"src-addr"`
		URL       string `yaml:"url"`
	} `yaml:"stream-domain"`
	StreamIpPort []struct {
		Type      string `yaml:"type"`
		Interface string `yaml:"interface"`
		Nexthop   string `yaml:"nexthop"`
		SrcAddr   string `yaml:"src-addr"`
		IpGroup   string `yaml:"ip-group"`
	} `yaml:"stream-ipport"`
}

// 临时存储新配置的数据结构
type customIspData struct {
	name     string
	ipGroups [][]string
}

type ipGroupData struct {
	name     string
	ipGroups [][]string
}

type streamDomainData struct {
	iface   string
	srcAddr string
	domains [][]string
}

type streamIpPortData struct {
	type_        string
	iface        string
	nexthop      string
	srcAddr      string
	ipGroupList  []string
}

func main() {
	flag.Parse()

	err := readConf(*confPath)
	if err != nil {
		log.Println("读取配置文件失败：", err)
		return
	}

	update()

	if conf.Cron == "" {
		return
	}

	c := cron.New()
	_, err = c.AddFunc(conf.Cron, update)
	if err != nil {
		log.Println("启动计划任务失败：", err)
		return
	} else {
		log.Println("已启动计划任务")
	}
	c.Start()

	{
		osSignals := make(chan os.Signal, 1)
		signal.Notify(osSignals, os.Interrupt, os.Kill, syscall.SIGTERM)
		<-osSignals
	}
}

func readConf(filename string) error {
	buf, err := os.ReadFile(filename)
	if err != nil {
		return err
	}
	err = yaml.Unmarshal(buf, &conf)
	if err != nil {
		return fmt.Errorf("in file %q: %v", filename, err)
	}
	return nil
}

func update() {
	err := readConf(*confPath)
	if err != nil {
		log.Println("更新配置文件失败：", err)
		return
	}

	baseurl := conf.IkuaiURL
	if baseurl == "" {
		gateway, err := router.GetGateway()
		if err != nil {
			log.Println("获取默认网关失败：", err)
			return
		}
		baseurl = "http://" + gateway
		log.Println("使用默认网关地址：", baseurl)
	}

	iKuai := api.NewIKuai(baseurl)

	err = iKuai.Login(conf.Username, conf.Password)
	if err != nil {
		log.Println("登陆失败：", err)
		return
	} else {
		log.Println("登录成功")
	}

	// 1. 先获取所有新配置
	newCustomIsps, err := fetchAllCustomIsp()
	if err != nil {
		log.Println("获取自定义运营商配置失败，终止更新：", err)
		return
	}

	newIpGroups, err := fetchAllIpGroup()
	if err != nil {
		log.Println("获取IP分组配置失败，终止更新：", err)
		return
	}

	newStreamDomains, err := fetchAllStreamDomain()
	if err != nil {
		log.Println("获取域名分流配置失败，终止更新：", err)
		return
	}

	newStreamIpPorts, err := fetchAllStreamIpPort(iKuai)
	if err != nil {
		log.Println("获取端口分流配置失败，终止更新：", err)
		return
	}

	// 2. 所有新配置获取成功后，删除旧配置
	err = iKuai.DelIKuaiBypassCustomIsp()
	if err != nil {
		log.Println("移除旧的自定义运营商失败：", err)
	} else {
		log.Println("移除旧的自定义运营商成功")
	}

	err = iKuai.DelIKuaiBypassIpGroup()
	if err != nil {
		log.Println("移除旧的IP分组失败：", err)
	} else {
		log.Println("移除旧的IP分组成功")
	}

	err = iKuai.DelIKuaiBypassStreamDomain()
	if err != nil {
		log.Println("移除旧的域名分流失败：", err)
	} else {
		log.Println("移除旧的域名分流成功")
	}

	err = iKuai.DelIKuaiBypassStreamIpPort()
	if err != nil {
		log.Println("移除旧的端口分流失败：", err)
	} else {
		log.Println("移除旧的端口分流成功")
	}

	// 3. 应用新配置
	applyCustomIsps(iKuai, newCustomIsps)
	applyIpGroups(iKuai, newIpGroups)
	applyStreamDomains(iKuai, newStreamDomains)
	applyStreamIpPorts(iKuai, newStreamIpPorts)
}

// 获取所有自定义运营商新配置
func fetchAllCustomIsp() ([]customIspData, error) {
	var result []customIspData
	for _, cfg := range conf.CustomIsp {
		resp, err := http.Get(cfg.URL)
		if err != nil {
			return nil, fmt.Errorf("获取%s配置失败: %v", cfg.Name, err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != 200 {
			return nil, fmt.Errorf("%s返回状态码: %d", cfg.URL, resp.StatusCode)
		}

		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, fmt.Errorf("读取%s内容失败: %v", cfg.Name, err)
		}

		ips := strings.Split(string(body), "\n")
		ips = removeIpv6(ips)
		ipGroups := group(ips, 5000)

		result = append(result, customIspData{
			name:     cfg.Name,
			ipGroups: ipGroups,
		})
	}
	return result, nil
}

// 获取所有IP分组新配置
func fetchAllIpGroup() ([]ipGroupData, error) {
	var result []ipGroupData
	for _, cfg := range conf.IpGroup {
		resp, err := http.Get(cfg.URL)
		if err != nil {
			return nil, fmt.Errorf("获取%s配置失败: %v", cfg.Name, err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != 200 {
			return nil, fmt.Errorf("%s返回状态码: %d", cfg.URL, resp.StatusCode)
		}

		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, fmt.Errorf("读取%s内容失败: %v", cfg.Name, err)
		}

		ips := strings.Split(string(body), "\n")
		ips = removeIpv6(ips)
		ipGroups := group(ips, 1000)

		result = append(result, ipGroupData{
			name:     cfg.Name,
			ipGroups: ipGroups,
		})
	}
	return result, nil
}

// 获取所有域名分流新配置
func fetchAllStreamDomain() ([]streamDomainData, error) {
	var result []streamDomainData
	for _, cfg := range conf.StreamDomain {
		resp, err := http.Get(cfg.URL)
		if err != nil {
			return nil, fmt.Errorf("获取%s配置失败: %v", cfg.URL, err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != 200 {
			return nil, fmt.Errorf("%s返回状态码: %d", cfg.URL, resp.StatusCode)
		}

		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, fmt.Errorf("读取%s内容失败: %v", cfg.URL, err)
		}

		domains := strings.Split(string(body), "\n")
		domainGroups := group(domains, 1000)

		result = append(result, streamDomainData{
			iface:   cfg.Interface,
			srcAddr: cfg.SrcAddr,
			domains: domainGroups,
		})
	}
	return result, nil
}

// 获取所有端口分流新配置
func fetchAllStreamIpPort(iKuai *api.IKuai) ([]streamIpPortData, error) {
	var result []streamIpPortData
	for _, cfg := range conf.StreamIpPort {
		var ipGroupList []string
		for _, ipGroupItem := range strings.Split(cfg.IpGroup, ",") {
			data, err := iKuai.GetAllIKuaiBypassIpGroupNamesByName(ipGroupItem)
			if err != nil {
				return nil, fmt.Errorf("获取IP分组%s失败: %v", ipGroupItem, err)
			}
			ipGroupList = append(ipGroupList, data...)
		}

		result = append(result, streamIpPortData{
			type_:       cfg.Type,
			iface:       cfg.Interface,
			nexthop:     cfg.Nexthop,
			srcAddr:     cfg.SrcAddr,
			ipGroupList: ipGroupList,
		})
	}
	return result, nil
}

// 应用自定义运营商配置
func applyCustomIsps(iKuai *api.IKuai, dataList []customIspData) {
	for _, data := range dataList {
		for _, ig := range data.ipGroups {
			ipGroup := strings.Join(ig, ",")
			if err := iKuai.AddCustomIsp(data.name, ipGroup); err != nil {
				log.Printf("添加自定义运营商'%s'失败: %v", data.name, err)
			}
		}
		log.Printf("添加自定义运营商'%s'成功", data.name)
	}
}

// 应用IP分组配置
func applyIpGroups(iKuai *api.IKuai, dataList []ipGroupData) {
	for _, data := range dataList {
		for index, ig := range data.ipGroups {
			ipGroup := strings.Join(ig, ",")
			name := data.name + "_" + strconv.Itoa(index)
			if err := iKuai.AddIpGroup(name, ipGroup); err != nil {
				log.Printf("添加IP分组'%s'失败: %v", name, err)
			}
		}
		log.Printf("添加IP分组'%s'成功", data.name)
	}
}

// 应用域名分流配置
func applyStreamDomains(iKuai *api.IKuai, dataList []streamDomainData) {
	for _, data := range dataList {
		for _, d := range data.domains {
			domain := strings.Join(d, ",")
			if err := iKuai.AddStreamDomain(data.iface, data.srcAddr, domain); err != nil {
				log.Printf("添加域名分流'%s'失败: %v", data.iface, err)
			}
		}
		log.Printf("添加域名分流'%s'成功", data.iface)
	}
}

// 应用端口分流配置
func applyStreamIpPorts(iKuai *api.IKuai, dataList []streamIpPortData) {
	for _, data := range dataList {
		if err := iKuai.AddStreamIpPort(
			data.type_,
			data.iface,
			strings.Join(data.ipGroupList, ","),
			data.srcAddr,
			data.nexthop,
		); err != nil {
			log.Printf("添加端口分流'%s'失败: %v", data.iface, err)
		}
		log.Printf("添加端口分流'%s'成功", data.iface)
	}
}

func removeIpv6(ips []string) []string {
	i := 0
	for _, ip := range ips {
		if !strings.Contains(ip, ":") {
			ips[i] = ip
			i++
		}
	}
	return ips[:i]
}

func group(arr []string, subGroupLength int64) [][]string {
	max := int64(len(arr))
	var segmens = make([][]string, 0)
	quantity := max / subGroupLength
	remainder := max % subGroupLength
	i := int64(0)
	for i = int64(0); i < quantity; i++ {
		segmens = append(segmens, arr[i*subGroupLength:(i+1)*subGroupLength])
	}
	if quantity == 0 || remainder != 0 {
		segmens = append(segmens, arr[i*subGroupLength:i*subGroupLength+remainder])
	}
	return segmens
}