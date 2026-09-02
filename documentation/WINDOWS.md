## Trouble shooting common issues with Windows


### RAM not reporting

If you are seeing a static number with the maximum allocated RAM you are likely missing the Ballooning driver from the VirtIO package. 

For some reason it does not seem to be installed fully by default (or atleast it did not for me), as such the RAM usage does not get reported back through the QEMU guest agent to be read by libvirt. 

To fix this issue re-run the VirtIO installation file and be sure to select "Entire feature will be installed on local hard drive" for the Ballooning driver.

Once installed Pademelon will be able to recieve the RAM usage statistics. 

![Installing VirtIO Drivers](https://github.com/perkyquirky/Pademelon/blob/main/images/install-balloon.png?raw=true)

You can verify the driver is running by looking at running services:

![Balloon Service Running](https://github.com/perkyquirky/Pademelon/blob/main/images/balloon-service-1.png?raw=true)

![Balloon Serice in Services](https://github.com/perkyquirky/Pademelon/blob/main/images/balloon-service-2.png?raw=true)

![Balloon Serice Process](https://github.com/perkyquirky/Pademelon/blob/main/images/balloon-process.png?raw=true)
