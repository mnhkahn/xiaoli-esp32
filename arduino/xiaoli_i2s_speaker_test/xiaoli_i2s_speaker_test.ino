/*
  Based on Arduino-ESP32 ESP_I2S/examples/Simple_tone.
  Only the I2S pins are changed for the Xiaoli ESP32-S3 MAX98357A speaker.
*/

#include <Arduino.h>
#include <ESP_I2S.h>

#define I2S_LRC  GPIO_NUM_41
#define I2S_BCLK GPIO_NUM_40
#define I2S_DIN  GPIO_NUM_39

const int frequency = 440;
const int amplitude = 500;
const int sampleRate = 8000;

i2s_data_bit_width_t bps = I2S_DATA_BIT_WIDTH_16BIT;
i2s_mode_t mode = I2S_MODE_STD;
i2s_slot_mode_t slot = I2S_SLOT_MODE_STEREO;

const unsigned int halfWavelength = sampleRate / frequency / 2;

int32_t sample = amplitude;
unsigned int count = 0;

I2SClass i2s;

void setup() {
  Serial.begin(115200);
  delay(1000);
  Serial.println("I2S simple tone");
  Serial.printf("pins: BCLK=%d LRC=%d DIN=%d\n", I2S_BCLK, I2S_LRC, I2S_DIN);

  i2s.setPins(I2S_BCLK, I2S_LRC, I2S_DIN);

  if (!i2s.begin(mode, sampleRate, bps, slot)) {
    Serial.println("Failed to initialize I2S!");
    while (1) {
      delay(1000);
    }
  }

  Serial.println("I2S started");
}

void loop() {
  if (count % halfWavelength == 0) {
    sample = -1 * sample;
  }

  i2s.write(sample);
  i2s.write(sample >> 8);

  i2s.write(sample);
  i2s.write(sample >> 8);

  count++;
}
