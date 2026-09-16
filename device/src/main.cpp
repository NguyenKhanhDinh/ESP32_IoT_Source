#include <Arduino.h>
#include <WiFi.h>
#include <PubSubClient.h>
#include <DHT.h>
#include <math.h>

#define DHT_PIN 15
#define DHT_TYPE DHT22
#define LDR_PIN 34

const char* WIFI_SSID = "Wokwi-GUEST";
const char* WIFI_PASSWORD = "";

const char* MQTT_SERVER = "host.wokwi.internal";
const int MQTT_PORT = 1883;

const char* MQTT_TOPIC = "iot/sensors";

DHT dht(DHT_PIN, DHT_TYPE);

WiFiClient espClient;
PubSubClient mqttClient(espClient);

void connectWiFi() {
    Serial.print("Connecting to WiFi");

    WiFi.begin(WIFI_SSID, WIFI_PASSWORD);

    while (WiFi.status() != WL_CONNECTED) {
        delay(500);
        Serial.print(".");
    }

    Serial.println();
    Serial.println("WiFi connected!");
    Serial.print("ESP32 IP: ");
    Serial.println(WiFi.localIP());
}

void connectMQTT() {
    while (!mqttClient.connected()) {
        Serial.print("Connecting to MQTT...");

        String clientId = "ESP32-Simulator-" + String(random(0xffff), HEX);

        if (mqttClient.connect(clientId.c_str())) {
            Serial.println("connected!");
        } else {
            Serial.print("failed, state=");
            Serial.println(mqttClient.state());
            delay(2000);
        }
    }
}

void setup() {
    Serial.begin(115200);

    dht.begin();

    connectWiFi();

    mqttClient.setServer(MQTT_SERVER, MQTT_PORT);

    Serial.println("ESP32 IoT Sensor Starting...");
}

void loop() {

    if (WiFi.status() != WL_CONNECTED) {
        connectWiFi();
    }

    if (!mqttClient.connected()) {
        connectMQTT();
    }

    mqttClient.loop();

    // =========================
    // Đọc DHT22
    // =========================

    float humidity = dht.readHumidity();
    float temperature = dht.readTemperature();

    if (isnan(humidity) || isnan(temperature)) {
        Serial.println("Failed to read DHT22!");
        delay(2000);
        return;
    }

    // =========================
    // Đọc LDR
    // =========================

    int analogValue = analogRead(LDR_PIN);

    // ESP32 ADC: 12-bit -> 0..4095
    const float ADC_MAX = 4095.0;
    const float VCC = 3.3;

    float voltage = analogValue * VCC / ADC_MAX;

    // =========================
    // Tính điện trở LDR
    // =========================

    const float R_FIXED = 10000.0;  // 10 kΩ

    float resistance = 0;

    if (voltage > 0.001 && voltage < VCC) {
        resistance = R_FIXED * voltage / (VCC - voltage);
    }

    // =========================
    // Điện trở LDR -> Lux
    // =========================

    // Tham số mặc định của Wokwi photoresistor:
    // rl10 = 50 kΩ tại 10 lux
    // gamma = 0.7

    const float RL10 = 50.0;   // kΩ
    const float GAMMA = 0.7;

    float lux = 0;

    if (resistance > 0) {

        lux = pow(
            RL10 * 1000.0 *
            pow(10.0, GAMMA) /
            resistance,
            1.0 / GAMMA
        );
    }

    // Kiểm tra giá trị hợp lệ
    if (!isfinite(lux)) {
        lux = 100000;
    }

    if (lux < 0) {
        lux = 0;
    }

    if (lux > 100000) {
        lux = 100000;
    }

    // =========================
    // In Serial
    // =========================

    Serial.println("--------------------");

    Serial.print("Temperature: ");
    Serial.print(temperature, 2);
    Serial.println(" °C");

    Serial.print("Humidity: ");
    Serial.print(humidity, 2);
    Serial.println(" %");

    Serial.print("LDR ADC: ");
    Serial.println(analogValue);

    Serial.print("LDR Voltage: ");
    Serial.print(voltage, 3);
    Serial.println(" V");

    Serial.print("LDR Resistance: ");
    Serial.print(resistance, 2);
    Serial.println(" ohm");

    Serial.print("Light intensity: ");
    Serial.print(lux, 2);
    Serial.println(" lux");

    // =========================
    // MQTT JSON
    // =========================

    String payload = "{";

    payload += "\"temperature\":" + String(temperature, 2) + ",";
    payload += "\"humidity\":" + String(humidity, 2) + ",";
    payload += "\"light\":" + String(lux, 2);

    payload += "}";

    Serial.print("MQTT publish: ");
    Serial.println(payload);

    if (mqttClient.publish(MQTT_TOPIC, payload.c_str())) {
        Serial.println("MQTT publish successful!");
    } else {
        Serial.println("MQTT publish failed!");
    }

    delay(2000);
}